package consumer

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"gatewai/gateway/internal/config"
	"gatewai/gateway/internal/metrics"
	"gatewai/gateway/internal/model"
	"gatewai/gateway/internal/storage"
)

// S3Store is the subset of *storage.S3Client used by WebhookSender.
// Using an interface keeps the package testable without a real S3 server.
type S3Store interface {
	GetObject(ctx context.Context, objectKey string) ([]byte, error)
	DeleteObject(ctx context.Context, objectKey string) error
	// Upload stores objectKey (used by async result-stage redaction to write a
	// redacted sibling object). size is the byte length of the reader's content.
	Upload(ctx context.Context, objectKey string, reader io.Reader, size int64, contentType string) error
}

// WebhookSender delivers job result notifications to client callback URLs.
//
// Delivery is durable: Send makes one inline attempt, and on failure the retry
// is persisted to a Redis ZSET (webhook:retries) worked by RunRetryLoop, so a
// gateway restart never drops pending retries. After MaxRetries attempts a
// webhook is dead-lettered to the webhook:deadletter list. Construct with
// NewWebhookSender; call Send in a goroutine per job and RunRetryLoop once.
type WebhookSender struct {
	redis          *storage.RedisClient
	s3             S3Store
	httpClient     *http.Client
	persistsResult atomic.Bool

	maxRetries    int
	baseBackoff   time.Duration
	maxBackoff    time.Duration
	signingSecret string // empty = unsigned
}

// NewWebhookSender creates a WebhookSender.
// persistsResult controls whether the S3 result object is deleted after
// successful webhook delivery (false = delete, true = keep).
func NewWebhookSender(
	redis *storage.RedisClient,
	s3 S3Store,
	persistsResult bool,
	cfg config.WebhookConfig,
) *WebhookSender {
	ws := &WebhookSender{
		redis: redis,
		s3:    s3,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		maxRetries:    cfg.MaxRetriesOrDefault(),
		baseBackoff:   cfg.RetryBackoffDuration(),
		maxBackoff:    cfg.MaxBackoffDuration(),
		signingSecret: cfg.SigningSecret,
	}
	ws.persistsResult.Store(persistsResult)
	return ws
}

// UpdatePersistsResult updates the S3 result retention policy at runtime.
// Safe to call concurrently with in-flight Send goroutines.
func (ws *WebhookSender) UpdatePersistsResult(v bool) {
	ws.persistsResult.Store(v)
}

// webhookPayload is the body posted to the client's callback URL.
type webhookPayload struct {
	JobID       string          `json:"job_id"`
	ServiceType string          `json:"service_type"`
	Status      model.JobStatus `json:"status"`
	Result      json.RawMessage `json:"result,omitempty"` // inline result payload when completed
	Error       string          `json:"error,omitempty"`
	CompletedAt time.Time       `json:"completed_at"`
}

// Send delivers the webhook for job. Intended to be called in its own goroutine.
// It renders the payload (fetching the result from S3 when present), makes one
// inline delivery attempt, and — if that fails — enqueues a durable retry.
func (ws *WebhookSender) Send(job *model.Job) {
	// Restore trace context from the job so this span is a child of the original
	// submit request even though delivery happens asynchronously.
	ctx := context.Background()
	if job.TraceContext != "" {
		carrier := propagation.MapCarrier{"traceparent": job.TraceContext}
		ctx = otel.GetTextMapPropagator().Extract(ctx, carrier)
	}
	ctx, span := otel.Tracer("gatewai/gateway").Start(ctx, "gateway.webhook.send",
		trace.WithAttributes(
			attribute.String("job_id", job.ID),
			attribute.String("service_type", job.ServiceType),
			attribute.String("job_status", string(job.Status)),
		))
	defer span.End()

	payload := ws.buildPayload(ctx, job)

	if ok, code := ws.deliver(ctx, job.CallbackURL, payload, job.ID); ok {
		slog.Info("webhook delivered", "job_id", job.ID, "status_code", code, "attempt", 1)
		ws.onDelivered(job.ID, job.ResultRef)
		return
	}
	slog.Warn("webhook first attempt failed, scheduling durable retry", "job_id", job.ID)
	ws.enqueueRetry(ctx, &webhookTask{
		JobID:       job.ID,
		ServiceType: job.ServiceType,
		URL:         job.CallbackURL,
		Payload:     payload,
		Attempt:     1,
		ResultRef:   job.ResultRef,
	})
}

// buildPayload renders the webhook body, inlining the S3 result when completed.
func (ws *WebhookSender) buildPayload(ctx context.Context, job *model.Job) []byte {
	p := webhookPayload{
		JobID:       job.ID,
		ServiceType: job.ServiceType,
		Status:      job.Status,
		Error:       job.Error,
		CompletedAt: job.UpdatedAt,
	}
	if job.Status == model.JobStatusCompleted && job.ResultRef != "" {
		if data, err := ws.s3.GetObject(ctx, job.ResultRef); err != nil {
			slog.Error("webhook: failed to fetch result", "job_id", job.ID, "error", err)
		} else {
			p.Result = json.RawMessage(data)
		}
	}
	payload, _ := json.Marshal(p)
	return payload
}

// deliver POSTs the payload once. ok is true when the endpoint acknowledged the
// delivery — a network success with status < 500 (4xx is the client's concern,
// not retried). A network error or a 5xx returns ok=false (retryable).
func (ws *WebhookSender) deliver(ctx context.Context, url string, payload []byte, jobID string) (ok bool, statusCode int) {
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		slog.Error("webhook: building request", "job_id", jobID, "error", err)
		return false, 0
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Job-ID", jobID)
	if ws.signingSecret != "" {
		req.Header.Set("X-Gatewai-Signature", signWebhook(ws.signingSecret, payload))
	}

	resp, err := ws.httpClient.Do(req)
	if err != nil {
		slog.Warn("webhook request failed", "job_id", jobID, "error", err)
		return false, 0
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		slog.Warn("webhook server error", "job_id", jobID, "status_code", resp.StatusCode)
		return false, resp.StatusCode
	}
	return true, resp.StatusCode
}

// signWebhook returns the X-Gatewai-Signature header value for payload, using a
// fresh timestamp per call so consumers can reject replays. The signed content
// is "<unix_seconds>.<body>"; v1 is its HMAC-SHA256, hex-encoded.
//
// Consumer verification (pseudocode):
//
//	t, v1 = parse("t=…,v1=…")
//	reject if abs(now - t) > tolerance          # replay protection
//	expected = hex(hmac_sha256(secret, f"{t}.{raw_body}"))
//	accept if hmac_equal(expected, v1)
func signWebhook(secret string, payload []byte) string {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write(payload)
	return "t=" + ts + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

// onDelivered records a successful delivery and cleans up the S3 result when the
// retention policy says so.
func (ws *WebhookSender) onDelivered(jobID, resultRef string) {
	metrics.WebhookDeliveriesTotal.WithLabelValues("delivered").Inc()
	if !ws.persistsResult.Load() && resultRef != "" {
		if err := ws.s3.DeleteObject(context.Background(), resultRef); err != nil {
			slog.Error("webhook: failed to delete result file", "job_id", jobID, "error", err)
		}
	}
}
