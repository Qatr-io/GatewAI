// Package webhook delivers job completion notifications to client callback
// URLs from the relay — the process that handles exactly one job per pod
// invocation, making delivery exactly-once by construction (unlike the
// gateway's broadcast onComplete, ported from here).
package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"

	"gatewai/relay/internal/model"
)

const (
	maxRetries     = 3
	initialBackoff = 2 * time.Second
)

// ObjectStore is the subset of the relay's S3 client Send needs.
type ObjectStore interface {
	GetObject(ctx context.Context, key string) (io.ReadCloser, int64, string, error)
	DeleteObject(ctx context.Context, key string) error
}

type payload struct {
	JobID       string          `json:"job_id"`
	ServiceType string          `json:"service_type"`
	Status      model.JobStatus `json:"status"`
	Result      json.RawMessage `json:"result,omitempty"`
	Error       string          `json:"error,omitempty"`
	CompletedAt time.Time       `json:"completed_at"`
}

// Send delivers the webhook for job's completion (status/resultRef/errMsg are
// the values PublishResult was called with, not fields read off job — the
// relay's model.Job is not mutated during processing). Fetches the result
// from S3 when status is completed and resultRef is set, POSTs with
// retry/backoff, and optionally deletes the S3 object on success.
func Send(ctx context.Context, job *model.Job, status model.JobStatus, resultRef, errMsg string, s3 ObjectStore, httpClient *http.Client, persistsResult bool) {
	p := payload{
		JobID:       job.ID,
		ServiceType: job.ServiceType,
		Status:      status,
		Error:       errMsg,
		CompletedAt: time.Now().UTC(),
	}

	if status == model.JobStatusCompleted && resultRef != "" {
		body, _, _, err := s3.GetObject(ctx, resultRef)
		if err != nil {
			slog.ErrorContext(ctx, "webhook: failed to fetch result", "job_id", job.ID, "error", err)
		} else {
			data, err := io.ReadAll(body)
			body.Close()
			if err != nil {
				slog.ErrorContext(ctx, "webhook: failed to read result", "job_id", job.ID, "error", err)
			} else {
				p.Result = json.RawMessage(data)
			}
		}
	}

	data, _ := json.Marshal(p)
	backoff := initialBackoff

	for attempt := 1; attempt <= maxRetries; attempt++ {
		reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, job.CallbackURL, bytes.NewReader(data))
		if err != nil {
			cancel()
			slog.ErrorContext(ctx, "webhook: building request", "job_id", job.ID, "error", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Job-ID", job.ID)

		resp, err := httpClient.Do(req)
		cancel()

		if err == nil {
			resp.Body.Close()
			if resp.StatusCode < 500 {
				slog.InfoContext(ctx, "webhook delivered", "job_id", job.ID, "status_code", resp.StatusCode, "attempt", attempt)
				if !persistsResult && resultRef != "" {
					if derr := s3.DeleteObject(context.Background(), resultRef); derr != nil {
						slog.ErrorContext(ctx, "webhook: failed to delete result file", "job_id", job.ID, "error", derr)
					}
				}
				return
			}
			slog.WarnContext(ctx, "webhook server error", "job_id", job.ID, "status_code", resp.StatusCode, "attempt", attempt)
		} else {
			slog.WarnContext(ctx, "webhook request failed", "job_id", job.ID, "attempt", attempt, "error", err)
		}

		if attempt < maxRetries {
			time.Sleep(backoff)
			backoff *= 2
		}
	}

	slog.ErrorContext(ctx, "webhook failed after all retries", "job_id", job.ID, "url", job.CallbackURL)
}
