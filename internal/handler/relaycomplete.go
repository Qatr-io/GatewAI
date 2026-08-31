package handler

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/go-chi/chi/v5"

	"gatewai/gateway/internal/config"
	"gatewai/gateway/internal/consumer"
	"gatewai/gateway/internal/guardrails"
	"gatewai/gateway/internal/metrics"
	"gatewai/gateway/internal/model"
	"gatewai/gateway/internal/ratelimit"
	"gatewai/gateway/internal/service"
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
	s3            consumer.S3Store
	webhookSender *consumer.WebhookSender

	// reg drives result-stage (async) guardrails; nil = disabled. Swapped
	// atomically on config hot-reload (UpdateRegistry).
	reg atomic.Pointer[service.Registry]

	processingTimeLimiter ratelimit.ProcessingTimeChecker // nil = disabled
	tokenLimiter          ratelimit.TokenChecker          // nil = disabled
	usageTracker          usage.UsageTracker              // nil = no usage tracking

	wg sync.WaitGroup // tracks in-flight webhook + result-scan goroutines
}

// NewRelayCompleteHandler creates a RelayCompleteHandler.
// persistsResult controls whether the S3 result object is deleted after
// successful webhook delivery (false = delete, true = keep).
func NewRelayCompleteHandler(redis *storage.RedisClient, s3 consumer.S3Store, persistsResult bool, webhookCfg config.WebhookConfig) *RelayCompleteHandler {
	return &RelayCompleteHandler{
		redis:         redis,
		s3:            s3,
		webhookSender: consumer.NewWebhookSender(redis, s3, persistsResult, webhookCfg),
	}
}

// WithRegistry attaches the service registry so async job results can be scanned
// by result-stage guardrails. Without it, async result scanning is disabled.
func (h *RelayCompleteHandler) WithRegistry(reg *service.Registry) *RelayCompleteHandler {
	h.reg.Store(reg)
	return h
}

// UpdateRegistry swaps the registry used for async result guardrails at runtime
// (config hot-reload). Safe to call concurrently with in-flight completions.
func (h *RelayCompleteHandler) UpdateRegistry(reg *service.Registry) {
	h.reg.Store(reg)
}

// StartRetryLoop launches the durable webhook retry worker. Call once at startup;
// it stops when ctx is cancelled.
func (h *RelayCompleteHandler) StartRetryLoop(ctx context.Context) {
	go h.webhookSender.RunRetryLoop(ctx)
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

	// Result-stage (async) guardrails: scan the job's result out-of-band. Runs
	// once per job (this handler is invoked exactly once per completion, unlike
	// the pub/sub broadcast), detached from the request context since the relay's
	// call returns here.
	h.maybeScanResult(context.WithoutCancel(ctx), job)

	if job.CallbackURL != "" {
		h.wg.Add(1)
		go func() {
			defer h.wg.Done()
			h.webhookSender.Send(job)
		}()
	}
}

// maybeScanResult fires a shadow result-stage guardrail scan when the registry
// is attached and the job's service has async guardrails configured. Best-effort:
// it never blocks completion (this slice only observes).
func (h *RelayCompleteHandler) maybeScanResult(ctx context.Context, job *model.Job) {
	reg := h.reg.Load()
	if reg == nil || h.s3 == nil {
		return
	}
	if job.Status != model.JobStatusCompleted || job.ResultRef == "" {
		return
	}
	def, err := reg.RouteAsync(job.ServiceType, job.Model)
	if err != nil || def == nil || !def.Guardrails.Async.Enabled {
		return
	}
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		h.scanResult(ctx, def, job)
	}()
}

// scanResult fetches the job's result and runs the async-stage detectors over
// its text in shadow: every match is logged and metered, nothing is blocked or
// mutated. (Enforcement is a later slice.) The result is fetched here rather
// than reusing the webhook's fetch to keep the scan independent of webhook
// delivery; a persisted-result service may delete the object after delivery, so
// the scan reads it promptly.
func (h *RelayCompleteHandler) scanResult(ctx context.Context, def *service.Def, job *model.Job) {
	body, err := h.s3.GetObject(ctx, job.ResultRef)
	if err != nil {
		slog.WarnContext(ctx, "async guardrails: failed to fetch result",
			"job_id", job.ID, "result_ref", job.ResultRef, "error", err)
		return
	}
	texts := guardrails.ResultTexts(body)
	if len(texts) == 0 {
		return
	}
	stage := def.Guardrails.Async

	// Regex checks (shadow).
	if len(stage.Checks) > 0 {
		rd := guardrails.NewRegexDetector(stage.Checks...)
		if findings, _ := rd.Scan(ctx, texts); len(findings) > 0 {
			slog.WarnContext(ctx, "async job result flagged by regex guardrail (shadow)",
				"service_type", def.Type, "model", def.Model, "job_id", job.ID,
				"detector", "regex", "violations", guardrails.Categories(findings))
			metrics.GuardrailsAsyncTotal.WithLabelValues(def.Type, def.Model, "regex", "flagged").Inc()
		}
	}

	// Model-backed detectors (shadow) — every detector runs; Action ignored here.
	for _, res := range guardrails.EvaluateAll(ctx, stage.Models, texts) {
		result := "flagged"
		if res.Err != nil {
			result = "error"
		}
		slog.WarnContext(ctx, "async job result flagged by guardrail model (shadow)",
			"service_type", def.Type, "model", def.Model, "job_id", job.ID,
			"detector", res.Name, "violations", res.Categories, "error", res.Err)
		metrics.GuardrailsAsyncTotal.WithLabelValues(def.Type, def.Model, res.Name, result).Inc()
	}
}
