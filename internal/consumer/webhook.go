package consumer

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"gatewai/gateway/internal/model"
	"gatewai/gateway/internal/storage"
)

const (
	webhookMaxRetries     = 3
	webhookInitialBackoff = 2 * time.Second
)

// S3Store is the subset of *storage.S3Client used by WebhookSender.
// Using an interface keeps the package testable without a real S3 server.
type S3Store interface {
	GetObject(ctx context.Context, objectKey string) ([]byte, error)
	DeleteObject(ctx context.Context, objectKey string) error
}

// WebhookSender delivers job result notifications to client callback URLs.
// It retries up to webhookMaxRetries times with exponential backoff.
// Use NewWebhookSender to construct one; call Send in a goroutine per job.
type WebhookSender struct {
	redis          *storage.RedisClient
	s3             S3Store
	httpClient     *http.Client
	persistsResult atomic.Bool
}

// NewWebhookSender creates a WebhookSender.
// persistsResult controls whether the S3 result object is deleted after
// successful webhook delivery (false = delete, true = keep).
func NewWebhookSender(
	redis *storage.RedisClient,
	s3 S3Store,
	persistsResult bool,
) *WebhookSender {
	ws := &WebhookSender{
		redis: redis,
		s3:    s3,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
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
// It fetches the result from S3 (when present and job completed), POSTs it to
// job.CallbackURL with retry/backoff, and optionally cleans up the S3 object.
func (ws *WebhookSender) Send(job *model.Job) {
	p := webhookPayload{
		JobID:       job.ID,
		ServiceType: job.ServiceType,
		Status:      job.Status,
		Error:       job.Error,
		CompletedAt: job.UpdatedAt,
	}

	if job.Status == model.JobStatusCompleted && job.ResultRef != "" {
		data, err := ws.s3.GetObject(context.Background(), job.ResultRef)
		if err != nil {
			slog.Error("webhook: failed to fetch result", "job_id", job.ID, "error", err)
		} else {
			p.Result = json.RawMessage(data)
		}
	}

	payload, _ := json.Marshal(p)
	backoff := webhookInitialBackoff

	for attempt := 1; attempt <= webhookMaxRetries; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, job.CallbackURL, bytes.NewReader(payload))
		if err != nil {
			cancel()
			slog.Error("webhook: building request", "job_id", job.ID, "error", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Job-ID", job.ID)

		resp, err := ws.httpClient.Do(req)
		cancel()

		if err == nil {
			resp.Body.Close()
			if resp.StatusCode < 500 {
				slog.Info("webhook delivered", "job_id", job.ID, "status_code", resp.StatusCode, "attempt", attempt)
				if !ws.persistsResult.Load() && job.ResultRef != "" {
					if derr := ws.s3.DeleteObject(context.Background(), job.ResultRef); derr != nil {
						slog.Error("webhook: failed to delete result file", "job_id", job.ID, "error", derr)
					}
				}
				return
			}
			slog.Warn("webhook server error", "job_id", job.ID, "status_code", resp.StatusCode, "attempt", attempt)
		} else {
			slog.Warn("webhook request failed", "job_id", job.ID, "attempt", attempt, "error", err)
		}

		if attempt < webhookMaxRetries {
			time.Sleep(backoff)
			backoff *= 2
		}
	}

	slog.Error("webhook failed after all retries", "job_id", job.ID, "url", job.CallbackURL)
}
