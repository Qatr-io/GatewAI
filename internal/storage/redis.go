package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"gatewai/gateway/internal/config"
	"gatewai/gateway/internal/metrics"
	"gatewai/gateway/internal/model"
)

// RedisClient wraps go-redis with job-specific persistence helpers.
type RedisClient struct {
	client    *redis.Client
	lifecycle config.LifecycleConfig
}

func NewRedis(cfg config.RedisConfig, lc config.LifecycleConfig) (*RedisClient, error) {
	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.Addr,
		Password: cfg.Password,
		DB:       cfg.DB,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := rdb.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("connecting to redis at %q: %w", cfg.Addr, err)
	}

	return &RedisClient{client: rdb, lifecycle: lc}, nil
}

// ttlForStatus returns the Redis TTL to apply when storing a job with the given status.
// Falls back to Global, then to a 2h internal safety net for orphaned records.
func (r *RedisClient) ttlForStatus(status model.JobStatus) time.Duration {
	var d time.Duration
	switch status {
	case model.JobStatusPending, model.JobStatusProcessing:
		d = r.lifecycle.JobTTL.PendingDuration()
	case model.JobStatusCompleted:
		d = r.lifecycle.JobTTL.CompletedDuration()
	case model.JobStatusFailed:
		d = r.lifecycle.JobTTL.FailedDuration()
	case model.JobStatusCancelled:
		d = r.lifecycle.JobTTL.CancelledDuration()
		if d == 0 {
			d = r.lifecycle.JobTTL.FailedDuration()
		}
	}
	if d == 0 {
		d = r.lifecycle.JobTTL.GlobalDuration()
	}
	if d == 0 {
		d = 2 * time.Hour // safety net for orphaned/unread records
	}
	return d
}

func (r *RedisClient) Close() error {
	return r.client.Close()
}

// UpdateLifecycle replaces the lifecycle config used for TTL calculations.
// Called by the hot-reload path so changes to job_ttl take effect without restart.
func (r *RedisClient) UpdateLifecycle(lc config.LifecycleConfig) {
	r.lifecycle = lc
}

// Client returns the underlying *redis.Client for callers that need direct access
// (e.g. rate limiting via Lua scripts).
func (r *RedisClient) Client() *redis.Client { return r.client }

// Raw exposes the underlying go-redis client for subsystems that need
// generic Redis access (e.g. the LLM response cache).
func (r *RedisClient) Raw() *redis.Client { return r.client }

// Ping checks the Redis connection health. Returns an error if unavailable.
func (r *RedisClient) Ping(ctx context.Context) error {
	return r.client.Ping(ctx).Err()
}

// JobsExistBatch checks which job IDs from ids have a live record in Redis.
// Returns a presence map keyed by job ID. Returns an error if Redis is unavailable —
// callers must treat an error as "inventory unreliable" and skip any deletion.
func (r *RedisClient) JobsExistBatch(ctx context.Context, ids []string) (map[string]bool, error) {
	if len(ids) == 0 {
		return map[string]bool{}, nil
	}
	keys := make([]string, len(ids))
	for i, id := range ids {
		keys[i] = jobKey(id)
	}
	vals, err := r.client.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, fmt.Errorf("redis MGET for batch job existence check: %w", err)
	}
	result := make(map[string]bool, len(ids))
	for i, v := range vals {
		result[ids[i]] = v != nil
	}
	return result, nil
}

func jobKey(id string) string        { return "job:" + id }
func consumerKey(name string) string { return "consumer:" + name + ":jobs" }
func queueKey(model string) string   { return "queue:" + model }

// SaveJob persists the full job struct as a JSON blob with the configured TTL.
// If the job has a ConsumerName, the job ID is also added to the consumer's
// sorted set (score = Unix timestamp) so it can be listed via ListJobsByConsumer.
func (r *RedisClient) SaveJob(ctx context.Context, job *model.Job) (err error) {
	ctx, span := otel.Tracer("gatewai/gateway").Start(ctx, "gateway.redis.enqueue",
		trace.WithAttributes(
			attribute.String("job_id", job.ID),
			attribute.String("service_type", job.ServiceType),
			attribute.String("model", job.Model),
			attribute.String("queue", "relay:"+job.Model+":pending"),
		))
	defer func() {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()
	}()

	start := time.Now()
	data, err := json.Marshal(job)
	if err != nil {
		return fmt.Errorf("marshaling job %q: %w", job.ID, err)
	}

	ttl := r.ttlForStatus(job.Status)
	_, pipeErr := r.client.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		pipe.Set(ctx, jobKey(job.ID), data, ttl)
		pipe.ZAdd(ctx, queueKey(job.Model), redis.Z{
			Score:  float64(job.CreatedAt.Unix()),
			Member: job.ID,
		})
		pipe.Expire(ctx, queueKey(job.Model), ttl)
		if job.ConsumerName != "" {
			pipe.ZAdd(ctx, consumerKey(job.ConsumerName), redis.Z{
				Score:  float64(job.CreatedAt.Unix()),
				Member: job.ID,
			})
			pipe.Expire(ctx, consumerKey(job.ConsumerName), ttl)
		}
		if job.Priority {
			pipe.LPush(ctx, "relay:"+job.Model+":pending", job.ID)
		} else {
			pipe.RPush(ctx, "relay:"+job.Model+":pending", job.ID)
		}
		return nil
	})

	metrics.ObserveWithExemplar(ctx, metrics.RedisOperationDuration.WithLabelValues("save_job"), time.Since(start).Seconds())
	if pipeErr != nil {
		metrics.RedisErrorsTotal.WithLabelValues("save_job").Inc()
		err = fmt.Errorf("saving job %q: %w", job.ID, pipeErr)
		return err
	}
	return nil
}

// GetJob retrieves a job from Redis. Returns a descriptive error when not found.
func (r *RedisClient) GetJob(ctx context.Context, id string) (*model.Job, error) {
	start := time.Now()
	data, err := r.client.Get(ctx, jobKey(id)).Bytes()
	metrics.ObserveWithExemplar(ctx, metrics.RedisOperationDuration.WithLabelValues("get_job"), time.Since(start).Seconds())
	if err == redis.Nil {
		return nil, fmt.Errorf("job %q not found", id)
	}
	if err != nil {
		metrics.RedisErrorsTotal.WithLabelValues("get_job").Inc()
		return nil, fmt.Errorf("getting job %q: %w", id, err)
	}

	var job model.Job
	if err := json.Unmarshal(data, &job); err != nil {
		return nil, fmt.Errorf("unmarshaling job %q: %w", id, err)
	}
	return &job, nil
}

// deleteJobScript atomically removes a job and cleans up the consumer index.
// It reads the job JSON, extracts consumer_name (if any), removes the job ID
// from the consumer sorted set, then deletes the job key — all in one step.
var deleteJobScript = redis.NewScript(`
local data = redis.call('GET', KEYS[1])
if data then
    local ok, job = pcall(cjson.decode, data)
    if ok then
        if job['consumer_name'] and job['consumer_name'] ~= '' then
            redis.call('ZREM', 'consumer:' .. job['consumer_name'] .. ':jobs', ARGV[1])
        end
        if job['model'] and job['model'] ~= '' then
            redis.call('ZREM', 'queue:' .. job['model'], ARGV[1])
        end
    end
end
return redis.call('DEL', KEYS[1])
`)

// DeleteJob removes a job record from Redis and cleans up the consumer index.
func (r *RedisClient) DeleteJob(ctx context.Context, id string) error {
	start := time.Now()
	err := deleteJobScript.Run(ctx, r.client, []string{jobKey(id)}, id).Err()
	metrics.ObserveWithExemplar(ctx, metrics.RedisOperationDuration.WithLabelValues("delete_job"), time.Since(start).Seconds())
	if err != nil {
		metrics.RedisErrorsTotal.WithLabelValues("delete_job").Inc()
		return fmt.Errorf("deleting job %q: %w", id, err)
	}
	return nil
}

// ListJobsByConsumer returns up to limit jobs for the given consumer, ordered
// by most-recent-first (ZREVRANGE on the score = creation Unix timestamp).
// total is the full count before pagination.
func (r *RedisClient) ListJobsByConsumer(ctx context.Context, consumer string, limit, offset int64) ([]*model.Job, int64, error) {
	key := consumerKey(consumer)

	// Pipeline: ZCARD + ZREVRANGE in one round-trip.
	var total int64
	var ids []string
	_, err := r.client.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		cardCmd := pipe.ZCard(ctx, key)
		rangeCmd := pipe.ZRevRange(ctx, key, offset, offset+limit-1)
		_ = cardCmd
		_ = rangeCmd
		return nil
	})
	if err != nil && err != redis.Nil {
		return nil, 0, fmt.Errorf("listing jobs for consumer %q: %w", consumer, err)
	}
	// Re-run individually to get typed results (TxPipelined results are in cmds).
	pipe := r.client.Pipeline()
	cardCmd := pipe.ZCard(ctx, key)
	rangeCmd := pipe.ZRevRange(ctx, key, offset, offset+limit-1)
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return nil, 0, fmt.Errorf("listing jobs for consumer %q: %w", consumer, err)
	}
	total = cardCmd.Val()
	ids = rangeCmd.Val()

	if len(ids) == 0 {
		return nil, total, nil
	}

	// Batch-fetch all job records.
	keys := make([]string, len(ids))
	for i, id := range ids {
		keys[i] = jobKey(id)
	}
	vals, err := r.client.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, 0, fmt.Errorf("fetching jobs for consumer %q: %w", consumer, err)
	}

	jobs := make([]*model.Job, 0, len(vals))
	for i, v := range vals {
		if v == nil {
			// Job TTL expired but consumer index not yet cleaned — skip stale entry.
			continue
		}
		var job model.Job
		if err := json.Unmarshal([]byte(v.(string)), &job); err != nil {
			slog.Warn("skipping malformed job record", "id", ids[i], "error", err)
			continue
		}
		jobs = append(jobs, &job)
	}

	// Pipeline ZRANK for pending jobs to populate QueuePosition in one round-trip.
	rankPipe := r.client.Pipeline()
	rankCmds := make([]*redis.IntCmd, len(jobs))
	for i, job := range jobs {
		if job.Status == model.JobStatusPending {
			rankCmds[i] = rankPipe.ZRank(ctx, queueKey(job.Model), job.ID)
		}
	}
	if _, err := rankPipe.Exec(ctx); err != nil && err != redis.Nil {
		slog.Warn("queue position pipeline failed", "consumer", consumer, "error", err)
	} else {
		for i, job := range jobs {
			if rankCmds[i] == nil {
				continue
			}
			if rank, err := rankCmds[i].Result(); err == nil {
				pos := rank + 1
				job.QueuePosition = &pos
			}
		}
	}

	return jobs, total, nil
}

// updateJobScript atomically reads a job JSON, patches status/result_ref/error/updated_at,
// and re-writes it with the same TTL — avoiding the read-modify-write race of GetJob+SaveJob.
// Terminal states (completed, failed) are never overwritten: a relay that finishes processing
// a job that was already marked stale by the GC is silently discarded.
var updateJobScript = redis.NewScript(`
local data = redis.call('GET', KEYS[1])
if not data then
    return redis.error_reply('job not found: ' .. KEYS[1])
end
local job = cjson.decode(data)
if job['status'] == 'completed' or job['status'] == 'failed' or job['status'] == 'cancelled' then
    return redis.status_reply('OK')
end
job['status']     = ARGV[1]
job['result_ref'] = ARGV[2]
job['error']      = ARGV[3]
job['updated_at'] = ARGV[4]
redis.call('SET', KEYS[1], cjson.encode(job), 'EX', tonumber(ARGV[5]))
if job['model'] and job['model'] ~= '' then
    redis.call('ZREM', 'queue:' .. job['model'], string.sub(KEYS[1], 5))
end
return redis.status_reply('OK')
`)

// GetQueuePosition returns the 1-indexed position of a pending job in the model queue.
// Returns (0, false, nil) when the job is no longer in the queue (already processing or done).
func (r *RedisClient) GetQueuePosition(ctx context.Context, jobID, model string) (int64, bool, error) {
	rank, err := r.client.ZRank(ctx, queueKey(model), jobID).Result()
	if err == redis.Nil {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("getting queue position for job %q: %w", jobID, err)
	}
	return rank + 1, true, nil
}

// UpdateJobResult atomically updates a job's status, result reference, and error
// message after the inference worker publishes its result event.
// Uses a Lua script so the read-modify-write is executed without a race window.
func (r *RedisClient) UpdateJobResult(ctx context.Context, jobID string, status model.JobStatus, resultRef, errMsg string) error {
	updatedAt := time.Now().UTC().Format(time.RFC3339Nano)
	ttlSecs := int64(r.ttlForStatus(status).Seconds())
	start := time.Now()
	err := updateJobScript.Run(ctx, r.client,
		[]string{jobKey(jobID)},
		string(status), resultRef, errMsg, updatedAt, ttlSecs,
	).Err()
	metrics.ObserveWithExemplar(ctx, metrics.RedisOperationDuration.WithLabelValues("update_job"), time.Since(start).Seconds())
	if err != nil {
		metrics.RedisErrorsTotal.WithLabelValues("update_job").Inc()
		return fmt.Errorf("updating job %q in redis: %w", jobID, err)
	}
	return nil
}

// PopJob atomically moves a job ID from relay:{model}:pending to
// relay:{model}:processing using BLMOVE (blocks until a job is available
// or the context is cancelled).
func (r *RedisClient) PopJob(ctx context.Context, modelName string) (string, error) {
	pending := "relay:" + modelName + ":pending"
	processing := "relay:" + modelName + ":processing"
	val, err := r.client.BLMove(ctx, pending, processing, "LEFT", "RIGHT", 0).Result()
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("blmove %s: %w", pending, err)
	}
	return val, nil
}

// DoneJob removes a job ID from relay:{model}:processing after completion.
func (r *RedisClient) DoneJob(ctx context.Context, jobID, modelName string) error {
	if err := r.client.LRem(ctx, "relay:"+modelName+":processing", 1, jobID).Err(); err != nil {
		return fmt.Errorf("lrem processing %s: %w", jobID, err)
	}
	return nil
}

// MarkJobCancelled marks a job as cancelled regardless of its current state
// (pending or processing) and signals the relay to stop inference if running.
// The job record is kept in Redis until the GC TTL expires — it is NOT deleted.
// S3 input/result cleanup is left to the GC.
func (r *RedisClient) MarkJobCancelled(ctx context.Context, jobID, modelName string) error {
	if err := r.UpdateJobResult(ctx, jobID, model.JobStatusCancelled, "", "cancelled by client"); err != nil {
		return err
	}
	// Remove from pending list in case the relay hasn't popped it yet.
	if err := r.client.LRem(ctx, "relay:"+modelName+":pending", 1, jobID).Err(); err != nil {
		slog.Warn("failed to lrem from pending list", "job_id", jobID, "model", modelName, "error", err)
	}
	// Signal the relay to stop if it is currently processing this job.
	if err := r.client.Publish(ctx, "relay:"+modelName+":cancel", jobID).Err(); err != nil {
		slog.Warn("failed to publish relay cancel signal", "job_id", jobID, "model", modelName, "error", err)
	}
	return nil
}

// relayQueueStates are the Redis list suffixes a job passes through:
// relay:{model}:pending -> relay:{model}:processing (mirrors
// internal/metrics.relayQueueStates, which reports depth for the same lists).
var relayQueueStates = [2]string{"pending", "processing"}

// SweepOrphanedRelayQueueEntries removes job IDs from relay:{model}:pending and
// relay:{model}:processing whose Redis job record no longer exists.
//
// A relay pod that exits (os.Exit) before calling DoneJob — e.g. on an infra
// error reading the job or running inference — never removes its job ID from
// the processing list, and one-pod-per-job scaling means no later pod picks
// that job back up to finish the cleanup. The stale-pending sweep (phase 1)
// has the same gap for the pending list: it marks the job record failed but
// never LRems the queue entry. Both leave the entry stuck forever, inflating
// gatewai_relay_queue_depth even once processing has stopped.
//
// Once the job's TTL expires (job absent from Redis), its queue entry is
// unambiguously orphaned regardless of state, so this reuses the same
// existence check as the S3 orphan sweep (phase 2) rather than inspecting
// job status.
type RelayQueueSweepResult struct {
	Model string
	State string
	Count int
}

func (r *RedisClient) SweepOrphanedRelayQueueEntries(ctx context.Context, models []string) ([]RelayQueueSweepResult, error) {
	var removed []RelayQueueSweepResult
	for _, m := range models {
		for _, state := range relayQueueStates {
			key := "relay:" + m + ":" + state
			ids, err := r.client.LRange(ctx, key, 0, -1).Result()
			if err != nil {
				return removed, fmt.Errorf("lrange %s: %w", key, err)
			}
			if len(ids) == 0 {
				continue
			}

			exists, err := r.JobsExistBatch(ctx, ids)
			if err != nil {
				return removed, fmt.Errorf("checking job existence for %s: %w", key, err)
			}

			count := 0
			for _, id := range ids {
				if exists[id] {
					continue
				}
				if err := r.client.LRem(ctx, key, 1, id).Err(); err != nil {
					slog.Warn("GC: failed to remove orphaned relay queue entry", "key", key, "job_id", id, "error", err)
					continue
				}
				count++
				slog.Info("GC: removed orphaned relay queue entry", "model", m, "state", state, "job_id", id)
			}
			if count > 0 {
				removed = append(removed, RelayQueueSweepResult{Model: m, State: state, Count: count})
			}
		}
	}
	return removed, nil
}

// scanStaleJobs returns all pending jobs whose queue score (creation Unix timestamp)
// is older than cutoff. Used by both SweepStalePendingJobs and ListStalePendingJobs
// to avoid duplicating the queue-scan + MGet logic.
func (r *RedisClient) scanStaleJobs(ctx context.Context, cutoff string) ([]*model.Job, error) {
	var queueKeys []string
	iter := r.client.Scan(ctx, 0, "queue:*", 0).Iterator()
	for iter.Next(ctx) {
		queueKeys = append(queueKeys, iter.Val())
	}
	if err := iter.Err(); err != nil {
		return nil, fmt.Errorf("scanning queue keys: %w", err)
	}

	var allIDs []string
	for _, key := range queueKeys {
		ids, err := r.client.ZRangeByScore(ctx, key, &redis.ZRangeBy{Min: "0", Max: cutoff}).Result()
		if err != nil {
			slog.Warn("failed to query stale jobs", "queue", key, "error", err)
			continue
		}
		allIDs = append(allIDs, ids...)
	}
	if len(allIDs) == 0 {
		return nil, nil
	}

	keys := make([]string, len(allIDs))
	for i, id := range allIDs {
		keys[i] = jobKey(id)
	}
	vals, err := r.client.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, fmt.Errorf("fetching stale jobs: %w", err)
	}

	jobs := make([]*model.Job, 0, len(vals))
	for i, v := range vals {
		if v == nil {
			continue
		}
		var job model.Job
		if err := json.Unmarshal([]byte(v.(string)), &job); err != nil {
			slog.Warn("skipping malformed job record during stale scan", "id", allIDs[i], "error", err)
			continue
		}
		if job.Status != model.JobStatusPending {
			continue
		}
		jobs = append(jobs, &job)
	}
	return jobs, nil
}

// SweepStalePendingJobs marks pending jobs older than maxAge as failed with
// reason "stale: pending too long" and returns the swept jobs so the caller
// can clean up their S3 input files. Called by the background GC goroutine.
func (r *RedisClient) SweepStalePendingJobs(ctx context.Context, maxAge time.Duration) ([]*model.Job, error) {
	cutoff := fmt.Sprintf("%d", time.Now().Add(-maxAge).Unix())

	jobs, err := r.scanStaleJobs(ctx, cutoff)
	if err != nil {
		return nil, err
	}

	swept := jobs[:0]
	for _, job := range jobs {
		if err := r.UpdateJobResult(ctx, job.ID, model.JobStatusFailed, "", "stale: pending too long"); err != nil {
			slog.Warn("failed to mark job stale", "job_id", job.ID, "error", err)
			continue
		}
		swept = append(swept, job)
		slog.Info("marked job stale", "job_id", job.ID, "model", job.Model)
	}
	return swept, nil
}

// ListStalePendingJobs returns all pending jobs older than olderThan.
// Used by the admin purge endpoint to retrieve jobs before deletion.
func (r *RedisClient) ListStalePendingJobs(ctx context.Context, olderThan time.Duration) ([]*model.Job, error) {
	cutoff := fmt.Sprintf("%d", time.Now().Add(-olderThan).Unix())
	return r.scanStaleJobs(ctx, cutoff)
}

// JobDoneSubscription is the interface returned by SubscribeJobDone.
// Callers block on Wait until the job completes, then call Close.
type JobDoneSubscription interface {
	Wait(ctx context.Context) error
	Close()
}

// jobDoneSub is the live Redis pub/sub implementation of JobDoneSubscription.
type jobDoneSub struct {
	ch     <-chan *redis.Message
	pubsub *redis.PubSub
}

func (s *jobDoneSub) Wait(ctx context.Context) error {
	select {
	case <-s.ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *jobDoneSub) Close() { _ = s.pubsub.Close() }

// SubscribeJobDone creates a subscription for the given job's completion channel.
// Must be called BEFORE the job is published to the relay queue to avoid missing the notification.
func (r *RedisClient) SubscribeJobDone(ctx context.Context, jobID string) JobDoneSubscription {
	ps := r.client.Subscribe(ctx, "job:"+jobID+":done")
	return &jobDoneSub{ch: ps.Channel(), pubsub: ps}
}

// NotifyJobDone publishes a signal on the job's completion channel to wake up
// any sync handler waiting via SubscribeJobDone.
func (r *RedisClient) NotifyJobDone(ctx context.Context, jobID string) {
	if err := r.client.Publish(ctx, "job:"+jobID+":done", "1").Err(); err != nil {
		slog.ErrorContext(ctx, "failed to notify job done", "job_id", jobID, "error", err)
	}
}

// reapProcessingScript atomically reclaims (or drops) one orphaned entry from a
// relay processing list. It re-checks the lease inside the script so it can
// never requeue a job a live worker still holds, and is idempotent across
// gateway replicas (LREM returns 0 → another replica already handled it).
//
//	KEYS[1] = lease key         KEYS[2] = processing list   KEYS[3] = pending list
//	KEYS[4] = dead-letter list  KEYS[5] = attempts counter
//	ARGV[1] = jobID  ARGV[2] = maxAttempts  ARGV[3] = attemptsTTLsec  ARGV[4] = mode ("reclaim"|"drop")
//
// Returns: alive | gone | dropped | requeued | deadletter
var reapProcessingScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 1 then return 'alive' end
if redis.call('LREM', KEYS[2], 1, ARGV[1]) == 0 then return 'gone' end
if ARGV[4] == 'drop' then return 'dropped' end
local n = redis.call('INCR', KEYS[5])
redis.call('EXPIRE', KEYS[5], tonumber(ARGV[3]))
if n > tonumber(ARGV[2]) then
    redis.call('RPUSH', KEYS[4], ARGV[1])
    return 'deadletter'
end
redis.call('RPUSH', KEYS[3], ARGV[1])
return 'requeued'
`)

// ReapResult summarises one reaper pass across all models.
type ReapResult struct {
	Requeued     int
	DeadLettered int
	Dropped      int
}

func isTerminalStatus(s model.JobStatus) bool {
	return s == model.JobStatusCompleted || s == model.JobStatusFailed || s == model.JobStatusCancelled
}

// ReapOrphanedProcessingJobs requeues jobs abandoned in relay:{model}:processing
// by a relay pod that died mid-job (OOM, node loss, SIGKILL) without releasing
// its lease. For each processing entry with no live lease:
//   - a live, non-terminal job record → requeued to relay:{model}:pending (up to
//     maxAttempts times, then dead-lettered to relay:{model}:deadletter and marked failed);
//   - a missing or terminal job record → dropped (stale processing entry cleaned up).
//
// The lease check is atomic with the reclaim, so a healthy worker's job is never
// requeued. Intended to run from the background GC loop.
func (r *RedisClient) ReapOrphanedProcessingJobs(ctx context.Context, maxAttempts int) (ReapResult, error) {
	var res ReapResult
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	const attemptsTTL = 24 * time.Hour

	var procKeys []string
	iter := r.client.Scan(ctx, 0, "relay:*:processing", 0).Iterator()
	for iter.Next(ctx) {
		procKeys = append(procKeys, iter.Val())
	}
	if err := iter.Err(); err != nil {
		return res, fmt.Errorf("scanning processing keys: %w", err)
	}

	for _, procKey := range procKeys {
		modelName := strings.TrimSuffix(strings.TrimPrefix(procKey, "relay:"), ":processing")
		ids, err := r.client.LRange(ctx, procKey, 0, -1).Result()
		if err != nil {
			slog.Warn("reaper: failed to LRANGE processing list", "key", procKey, "error", err)
			continue
		}
		for _, id := range ids {
			leaseK := "relay:" + modelName + ":lease:" + id
			// Fast path: a live lease means the worker is still processing.
			if n, err := r.client.Exists(ctx, leaseK).Result(); err == nil && n == 1 {
				continue
			}
			// Decide reclaim vs drop from the job record. Never drop on a Redis
			// error — we cannot distinguish "record gone" from "Redis down".
			mode := "reclaim"
			existsN, err := r.client.Exists(ctx, jobKey(id)).Result()
			if err != nil {
				slog.Warn("reaper: cannot verify job record, skipping", "job_id", id, "error", err)
				continue
			}
			if existsN == 0 {
				mode = "drop" // record TTL-expired — cannot be reprocessed
			} else if job, gerr := r.GetJob(ctx, id); gerr != nil {
				slog.Warn("reaper: failed to read job record, skipping", "job_id", id, "error", gerr)
				continue
			} else if isTerminalStatus(job.Status) {
				mode = "drop" // already completed/failed/cancelled
			}

			outcome, err := reapProcessingScript.Run(ctx, r.client,
				[]string{
					leaseK,
					procKey,
					"relay:" + modelName + ":pending",
					"relay:" + modelName + ":deadletter",
					"relay:" + modelName + ":attempts:" + id,
				},
				id, maxAttempts, int(attemptsTTL.Seconds()), mode,
			).Text()
			if err != nil {
				slog.Warn("reaper: reclaim script failed", "job_id", id, "error", err)
				continue
			}

			switch outcome {
			case "requeued":
				res.Requeued++
				metrics.AsyncJobsReapedTotal.WithLabelValues(modelName, "requeued").Inc()
				if err := r.UpdateJobResult(ctx, id, model.JobStatusPending, "", ""); err != nil {
					slog.Warn("reaper: failed to reset requeued job to pending", "job_id", id, "error", err)
				}
				slog.Info("reaper: requeued abandoned job", "job_id", id, "model", modelName)
			case "deadletter":
				res.DeadLettered++
				metrics.AsyncJobsReapedTotal.WithLabelValues(modelName, "deadletter").Inc()
				if err := r.UpdateJobResult(ctx, id, model.JobStatusFailed, "",
					fmt.Sprintf("relay worker died before completion; exceeded %d requeue attempts", maxAttempts)); err != nil {
					slog.Warn("reaper: failed to mark dead-lettered job failed", "job_id", id, "error", err)
				}
				slog.Warn("reaper: dead-lettered abandoned job", "job_id", id, "model", modelName, "max_attempts", maxAttempts)
			case "dropped":
				res.Dropped++
				metrics.AsyncJobsReapedTotal.WithLabelValues(modelName, "dropped").Inc()
				slog.Info("reaper: dropped stale processing entry", "job_id", id, "model", modelName)
			case "alive", "gone":
				// alive: lease reappeared between checks; gone: another replica won the race.
			}
		}
	}
	return res, nil
}

func idempotencyKey(consumer, key string) string {
	return "idem:" + consumer + ":" + key
}

// ReserveIdempotencyKey atomically claims an idempotency key for jobID (SET NX).
// Returns true if this caller won the claim (no prior submission with this key),
// false if the key was already taken. Scoped per consumer.
func (r *RedisClient) ReserveIdempotencyKey(ctx context.Context, consumer, key, jobID string, ttl time.Duration) (bool, error) {
	ok, err := r.client.SetNX(ctx, idempotencyKey(consumer, key), jobID, ttl).Result()
	if err != nil {
		return false, fmt.Errorf("reserve idempotency key: %w", err)
	}
	return ok, nil
}

// GetIdempotencyKey returns the job ID previously stored for an idempotency key,
// or "" if the key is absent.
func (r *RedisClient) GetIdempotencyKey(ctx context.Context, consumer, key string) (string, error) {
	v, err := r.client.Get(ctx, idempotencyKey(consumer, key)).Result()
	if err == redis.Nil {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get idempotency key: %w", err)
	}
	return v, nil
}

// ReleaseIdempotencyKey deletes an idempotency-key claim, letting a corrected
// retry proceed. Called when the reserved submission fails before the job is saved.
func (r *RedisClient) ReleaseIdempotencyKey(ctx context.Context, consumer, key string) error {
	if err := r.client.Del(ctx, idempotencyKey(consumer, key)).Err(); err != nil {
		return fmt.Errorf("release idempotency key: %w", err)
	}
	return nil
}
