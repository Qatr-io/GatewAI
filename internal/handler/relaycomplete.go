package handler

import (
	"fmt"
	"log/slog"
	"net/http"
	"sync"

	"github.com/go-chi/chi/v5"

	"gatewai/gateway/internal/consumer"
	"gatewai/gateway/internal/ratelimit"
	"gatewai/gateway/internal/storage"
	"gatewai/gateway/internal/usage"
)

// RelayCompleteHandler handles POST /-/relay/jobs/{id}/complete: a single,
// targeted call made by the relay after it persists a job's result to Redis.
// Unlike consumer.Manager's onComplete (which every gateway replica receives
// via Redis pub/sub broadcast), this handler is invoked exactly once per job
// via k8s Service load-balancing, so it is safe to perform rate-limit debit,
// usage tracking, and webhook delivery here.
type RelayCompleteHandler struct {
	redis         *storage.RedisClient
	webhookSender *consumer.WebhookSender

	processingTimeLimiter ratelimit.ProcessingTimeChecker // nil = disabled
	tokenLimiter          ratelimit.TokenChecker          // nil = disabled
	usageTracker          usage.UsageTracker               // nil = no usage tracking

	wg sync.WaitGroup // tracks in-flight webhook goroutines
}

// NewRelayCompleteHandler creates a RelayCompleteHandler.
// persistsResult controls whether the S3 result object is deleted after
// successful webhook delivery (false = delete, true = keep).
func NewRelayCompleteHandler(redis *storage.RedisClient, s3 consumer.S3Store, persistsResult bool) *RelayCompleteHandler {
	return &RelayCompleteHandler{
		redis:         redis,
		webhookSender: consumer.NewWebhookSender(redis, s3, persistsResult),
	}
}

// WithProcessingTimeLimiter attaches a processing-time budget limiter.
func (h *RelayCompleteHandler) WithProcessingTimeLimiter(l ratelimit.ProcessingTimeChecker) *RelayCompleteHandler {
	h.processingTimeLimiter = l
	return h
}

// WithTokenLimiter attaches a token budget limiter.
func (h *RelayCompleteHandler) WithTokenLimiter(l ratelimit.TokenChecker) *RelayCompleteHandler {
	h.tokenLimiter = l
	return h
}

// WithUsageTracker attaches a usage tracker.
func (h *RelayCompleteHandler) WithUsageTracker(t usage.UsageTracker) *RelayCompleteHandler {
	h.usageTracker = t
	return h
}

// UpdatePersistsResult updates the S3 result retention policy at runtime.
// Safe to call concurrently with in-flight webhook goroutines.
func (h *RelayCompleteHandler) UpdatePersistsResult(v bool) {
	h.webhookSender.UpdatePersistsResult(v)
}

// Wait drains all in-flight webhook goroutines. Call during graceful shutdown.
func (h *RelayCompleteHandler) Wait() {
	h.wg.Wait()
}

// Complete handles POST /-/relay/jobs/{id}/complete.
func (h *RelayCompleteHandler) Complete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	ctx := r.Context()

	job, err := h.redis.GetJob(ctx, id)
	if err != nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("job %q not found", id))
		return
	}

	if h.processingTimeLimiter != nil && job.ProcessingTime > 0 {
		if err := h.processingTimeLimiter.AddProcessingTime(ctx, job.ConsumerName, job.UserType, job.ServiceType, job.ProcessingTime); err != nil {
			slog.Error("relaycomplete: failed to add processing time", "job_id", id, "error", err)
		}
	}

	totalTokens := job.PromptTokens + job.CompletionTokens
	if h.tokenLimiter != nil && totalTokens > 0 {
		if err := h.tokenLimiter.AddTokensFor(ctx, job.ConsumerName, job.UserType, job.ServiceType, int(totalTokens)); err != nil {
			slog.Error("relaycomplete: failed to add tokens", "job_id", id, "error", err)
		}
	}

	if h.usageTracker != nil && job.ConsumerName != "" && job.ProcessingTime > 0 {
		h.usageTracker.TrackProcessingTime(ctx, job.ConsumerName, job.ServiceType, job.ProcessingTime)
		h.usageTracker.TrackActive(ctx, job.ConsumerName)
	}

	if h.usageTracker != nil && job.ConsumerName != "" && totalTokens > 0 {
		h.usageTracker.TrackTokens(ctx, job.ConsumerName, job.ServiceType, job.PromptTokens, job.CompletionTokens)
	}

	if h.usageTracker != nil && job.ConsumerName != "" {
		h.usageTracker.TrackUserType(ctx, job.ConsumerName, job.ServiceType, job.UserType)
	}

	w.WriteHeader(http.StatusOK)

	if job.CallbackURL != "" {
		h.wg.Add(1)
		go func() {
			defer h.wg.Done()
			h.webhookSender.Send(job)
		}()
	}
}
