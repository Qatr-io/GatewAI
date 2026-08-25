package consumer

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"gatewai/gateway/internal/metrics"
)

// Redis keys for the durable webhook retry subsystem.
const (
	webhookRetryZSet  = "webhook:retries"    // member = jobID, score = next-attempt unix ms
	webhookDeadLetter = "webhook:deadletter" // RPUSH of dead-lettered delivery summaries
)

func webhookTaskKey(jobID string) string { return "webhook:retry:" + jobID }

// webhookTask is the self-contained retry unit persisted at webhookTaskKey.
// It carries the rendered payload so retries survive job-record/S3 expiry.
type webhookTask struct {
	JobID       string `json:"job_id"`
	ServiceType string `json:"service_type"`
	URL         string `json:"url"`
	Payload     []byte `json:"payload"`
	Attempt     int    `json:"attempt"` // delivery attempts made so far
	ResultRef   string `json:"result_ref,omitempty"`
}

// webhookDeadLetterEntry is the (small) summary pushed to webhook:deadletter.
// The payload is intentionally omitted to keep the list compact.
type webhookDeadLetterEntry struct {
	JobID       string    `json:"job_id"`
	ServiceType string    `json:"service_type"`
	URL         string    `json:"url"`
	Attempts    int       `json:"attempts"`
	FailedAt    time.Time `json:"failed_at"`
}

// claimDueWebhooksScript atomically claims due retries: it returns up to N
// members whose score is <= now, bumping each to a future "in-flight" score so
// a crashed worker's claim reappears after the visibility timeout (no loss),
// and two gateway replicas never process the same task concurrently.
//
//	KEYS[1] = zset   ARGV[1] = nowMs   ARGV[2] = limit   ARGV[3] = visibilityMs
var claimDueWebhooksScript = redis.NewScript(`
local due = redis.call('ZRANGEBYSCORE', KEYS[1], '-inf', ARGV[1], 'LIMIT', 0, tonumber(ARGV[2]))
for _, m in ipairs(due) do
    redis.call('ZADD', KEYS[1], tonumber(ARGV[3]), m)
end
return due
`)

// taskTTL bounds how long a retry task key survives, well past the last retry.
func (ws *WebhookSender) taskTTL() time.Duration {
	return ws.maxBackoff*time.Duration(ws.maxRetries+2) + time.Hour
}

// backoff returns the delay before the given attempt number, exponential from
// baseBackoff and capped at maxBackoff.
func (ws *WebhookSender) backoff(attempt int) time.Duration {
	d := ws.baseBackoff
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= ws.maxBackoff {
			return ws.maxBackoff
		}
	}
	return d
}

// enqueueRetry persists a task and schedules its next attempt.
func (ws *WebhookSender) enqueueRetry(ctx context.Context, task *webhookTask) {
	data, err := json.Marshal(task)
	if err != nil {
		slog.Error("webhook: failed to marshal retry task", "job_id", task.JobID, "error", err)
		return
	}
	nextMs := time.Now().Add(ws.backoff(task.Attempt)).UnixMilli()
	rdb := ws.redis.Raw()
	pipe := rdb.TxPipeline()
	pipe.Set(ctx, webhookTaskKey(task.JobID), data, ws.taskTTL())
	pipe.ZAdd(ctx, webhookRetryZSet, redis.Z{Score: float64(nextMs), Member: task.JobID})
	if _, err := pipe.Exec(ctx); err != nil {
		slog.Error("webhook: failed to enqueue retry", "job_id", task.JobID, "error", err)
	}
}

// RunRetryLoop drains due webhook retries until ctx is cancelled. Start exactly
// one per gateway process (safe to run on every replica — claims are atomic).
func (ws *WebhookSender) RunRetryLoop(ctx context.Context) {
	const pollInterval = 5 * time.Second
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ws.processDueRetries(ctx)
		}
	}
}

// processDueRetries claims and processes all currently-due retries.
func (ws *WebhookSender) processDueRetries(ctx context.Context) {
	rdb := ws.redis.Raw()
	if n, err := rdb.ZCard(ctx, webhookRetryZSet).Result(); err == nil {
		metrics.WebhookRetryQueueDepth.Set(float64(n))
	}

	nowMs := time.Now().UnixMilli()
	visibilityMs := time.Now().Add(2 * time.Minute).UnixMilli()
	ids, err := claimDueWebhooksScript.Run(ctx, rdb,
		[]string{webhookRetryZSet}, nowMs, 100, visibilityMs).StringSlice()
	if err != nil {
		slog.Error("webhook: failed to claim due retries", "error", err)
		return
	}
	for _, jobID := range ids {
		ws.processRetry(ctx, jobID)
	}
}

// processRetry attempts one delivery of a claimed task and reschedules,
// dead-letters, or completes it.
func (ws *WebhookSender) processRetry(ctx context.Context, jobID string) {
	rdb := ws.redis.Raw()
	data, err := rdb.Get(ctx, webhookTaskKey(jobID)).Bytes()
	if err != nil {
		// Task key gone (TTL expired or already handled) — drop the zset entry.
		rdb.ZRem(ctx, webhookRetryZSet, jobID)
		return
	}
	var task webhookTask
	if err := json.Unmarshal(data, &task); err != nil {
		slog.Error("webhook: corrupt retry task, discarding", "job_id", jobID, "error", err)
		ws.removeTask(ctx, jobID)
		return
	}

	ok, code := ws.deliver(ctx, task.URL, task.Payload, task.JobID)
	task.Attempt++

	if ok {
		slog.Info("webhook delivered on retry", "job_id", jobID, "status_code", code, "attempt", task.Attempt)
		ws.onDelivered(jobID, task.ResultRef)
		ws.removeTask(ctx, jobID)
		return
	}

	if task.Attempt >= ws.maxRetries {
		entry, _ := json.Marshal(webhookDeadLetterEntry{
			JobID:       task.JobID,
			ServiceType: task.ServiceType,
			URL:         task.URL,
			Attempts:    task.Attempt,
			FailedAt:    time.Now().UTC(),
		})
		pipe := rdb.TxPipeline()
		pipe.RPush(ctx, webhookDeadLetter, entry)
		pipe.ZRem(ctx, webhookRetryZSet, jobID)
		pipe.Del(ctx, webhookTaskKey(jobID))
		if _, err := pipe.Exec(ctx); err != nil {
			slog.Error("webhook: failed to dead-letter", "job_id", jobID, "error", err)
			return
		}
		metrics.WebhookDeliveriesTotal.WithLabelValues("deadletter").Inc()
		slog.Error("webhook dead-lettered after all retries", "job_id", jobID, "url", task.URL, "attempts", task.Attempt)
		return
	}

	// Reschedule with backoff.
	updated, _ := json.Marshal(&task)
	nextMs := time.Now().Add(ws.backoff(task.Attempt)).UnixMilli()
	pipe := rdb.TxPipeline()
	pipe.Set(ctx, webhookTaskKey(jobID), updated, ws.taskTTL())
	pipe.ZAdd(ctx, webhookRetryZSet, redis.Z{Score: float64(nextMs), Member: jobID})
	if _, err := pipe.Exec(ctx); err != nil {
		slog.Error("webhook: failed to reschedule retry", "job_id", jobID, "error", err)
	}
}

// removeTask deletes a task key and its zset entry.
func (ws *WebhookSender) removeTask(ctx context.Context, jobID string) {
	rdb := ws.redis.Raw()
	pipe := rdb.TxPipeline()
	pipe.ZRem(ctx, webhookRetryZSet, jobID)
	pipe.Del(ctx, webhookTaskKey(jobID))
	_, _ = pipe.Exec(ctx)
}
