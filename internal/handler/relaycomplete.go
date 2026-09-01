package handler

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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

	// Result-stage (async) guardrails run before webhook delivery so a blocked
	// job is failed and a redacted result is repointed before the client is
	// notified. Both the scan and the webhook run in one background goroutine
	// (tracked for graceful shutdown), so the relay's ack above is not delayed
	// while scan-then-deliver ordering is preserved. When no async guardrails
	// apply this is exactly the previous behaviour (deliver the webhook).
	def := h.asyncGuardrailsFor(job)
	if def == nil && job.CallbackURL == "" {
		return
	}
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		bg := context.WithoutCancel(ctx)
		if def != nil {
			// Enforcing async guardrails gate the client-facing result: arm the
			// scan deadline now (the gate was armed with a zero deadline at submit),
			// run the scan, then clear the gate and wake sync waiters so the poll
			// path delivers only the scanned (block/redact) outcome.
			gated := def.Guardrails.Async.Enforcing()
			if gated {
				deadline := time.Now().Add(def.Guardrails.Async.ScanTimeout)
				if err := h.redis.SetScanGate(bg, job.ID, deadline, def.Guardrails.Async.OnTimeoutFailClosed); err != nil {
					slog.WarnContext(bg, "async guardrails: failed to arm scan deadline", "job_id", job.ID, "error", err)
				}
			}
			h.applyResultGuardrails(bg, def, job)
			if gated {
				if err := h.redis.ClearScanGate(bg, job.ID); err != nil {
					slog.WarnContext(bg, "async guardrails: failed to clear scan gate", "job_id", job.ID, "error", err)
				}
				h.redis.NotifyJobDone(bg, job.ID)
			}
		}
		if job.CallbackURL != "" {
			h.webhookSender.Send(job)
		}
	}()
}

// asyncGuardrailsFor returns the resolved service Def when the job is eligible
// for result-stage guardrails (registry attached, job completed with a result,
// and the service has an async guardrail stage), else nil.
func (h *RelayCompleteHandler) asyncGuardrailsFor(job *model.Job) *service.Def {
	reg := h.reg.Load()
	if reg == nil || h.s3 == nil {
		return nil
	}
	if job.Status != model.JobStatusCompleted || job.ResultRef == "" {
		return nil
	}
	def, err := reg.RouteAsync(job.ServiceType, job.Model)
	if err != nil || def == nil || !def.Guardrails.Async.Enabled {
		return nil
	}
	return def
}

// applyResultGuardrails fetches the job's result, runs the async-stage detectors
// over its text, and applies the stage action to any fired detector:
//   - flag   : observe only (log + metric), result delivered unchanged;
//   - block  : fail the job and clear its result, so the client receives a
//     failure notification and cannot fetch the flagged content;
//   - redact : rewrite the flagged spans, write the redacted result to a sibling
//     object, and repoint the job to it (the original is retained).
//
// It mutates job in place so the subsequent webhook reflects the outcome.
// Best-effort and fail-open: a detector error or an S3/Redis failure logs and
// leaves the result deliverable — a guardrail hiccup never fails a good job.
func (h *RelayCompleteHandler) applyResultGuardrails(ctx context.Context, def *service.Def, job *model.Job) {
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

	var fired []string // detector names that fired
	var cats []string  // union of categories across detectors

	if len(stage.Checks) > 0 {
		if findings, _ := guardrails.NewRegexDetector(stage.Checks...).Scan(ctx, texts); len(findings) > 0 {
			fired = append(fired, "regex")
			cats = append(cats, guardrails.Categories(findings)...)
		}
	}
	for _, res := range guardrails.EvaluateAll(ctx, stage.Models, texts) {
		if res.Err != nil {
			// Fail open on a detector error, even under block — a transient
			// model outage must not fail legitimate jobs.
			metrics.GuardrailsAsyncTotal.WithLabelValues(def.Type, def.Model, res.Name, "error").Inc()
			slog.WarnContext(ctx, "async guardrails: detector error (fail-open)",
				"job_id", job.ID, "detector", res.Name, "error", res.Err)
			continue
		}
		fired = append(fired, res.Name)
		cats = append(cats, res.Categories...)
	}
	if len(fired) == 0 {
		return
	}
	cats = dedupeStrings(cats)

	switch stage.Action {
	case guardrails.ActionBlock:
		h.blockResult(ctx, def, job, fired, cats)
	case guardrails.ActionRedact:
		h.redactResult(ctx, def, job, body, stage.Checks, fired, cats)
	default: // flag / shadow
		for _, d := range fired {
			metrics.GuardrailsAsyncTotal.WithLabelValues(def.Type, def.Model, d, "flagged").Inc()
		}
		slog.WarnContext(ctx, "async job result flagged by guardrails (shadow)",
			"service_type", def.Type, "model", def.Model, "job_id", job.ID, "detectors", fired, "violations", cats)
	}
}

// blockResult fails a completed job and clears its result so the flagged content
// is never delivered. The webhook that follows notifies the client of the failure.
func (h *RelayCompleteHandler) blockResult(ctx context.Context, def *service.Def, job *model.Job, fired, cats []string) {
	reason := "guardrails violation: " + strings.Join(cats, ", ")
	if err := h.redis.OverrideJobResult(ctx, job.ID, model.JobStatusFailed, "", reason); err != nil {
		slog.ErrorContext(ctx, "async guardrails: failed to fail blocked job (delivering result unchanged)",
			"job_id", job.ID, "error", err)
		return
	}
	job.Status = model.JobStatusFailed
	job.ResultRef = ""
	job.Error = reason
	for _, d := range fired {
		metrics.GuardrailsAsyncTotal.WithLabelValues(def.Type, def.Model, d, "blocked").Inc()
	}
	slog.WarnContext(ctx, "async job result blocked by guardrails",
		"service_type", def.Type, "model", def.Model, "job_id", job.ID, "detectors", fired, "violations", cats)
}

// redactResult rewrites the flagged spans in the result, writes the redacted
// body to a sibling object, and repoints the job to it (the original is kept for
// audit). Only regex groups can redact arbitrary result JSON (span-based); with
// no regex checks configured there is nothing to redact, so it degrades to flag.
func (h *RelayCompleteHandler) redactResult(ctx context.Context, def *service.Def, job *model.Job, body []byte, checks, fired, cats []string) {
	if len(checks) == 0 {
		for _, d := range fired {
			metrics.GuardrailsAsyncTotal.WithLabelValues(def.Type, def.Model, d, "flagged").Inc()
		}
		slog.WarnContext(ctx, "async guardrails: redact configured without regex checks — flagging only",
			"service_type", def.Type, "model", def.Model, "job_id", job.ID, "detectors", fired, "violations", cats)
		return
	}
	redacted, redCats := guardrails.RedactResultTexts(body, checks)
	if len(redCats) == 0 {
		return // classifiers fired but no regex spans to redact
	}
	siblingKey := job.ResultRef + ".redacted"
	if err := h.s3.Upload(ctx, siblingKey, bytes.NewReader(redacted), int64(len(redacted)), "application/json"); err != nil {
		slog.ErrorContext(ctx, "async guardrails: failed to write redacted result (delivering original)",
			"job_id", job.ID, "error", err)
		return
	}
	if err := h.redis.OverrideJobResult(ctx, job.ID, model.JobStatusCompleted, siblingKey, ""); err != nil {
		slog.ErrorContext(ctx, "async guardrails: failed to repoint redacted result (delivering original)",
			"job_id", job.ID, "error", err)
		return
	}
	job.ResultRef = siblingKey
	metrics.GuardrailsAsyncTotal.WithLabelValues(def.Type, def.Model, "regex", "redacted").Inc()
	slog.WarnContext(ctx, "async job result redacted by guardrails",
		"service_type", def.Type, "model", def.Model, "job_id", job.ID, "result_ref", siblingKey, "violations", redCats)
}

// dedupeStrings returns the input with duplicates removed, preserving order.
func dedupeStrings(in []string) []string {
	if len(in) < 2 {
		return in
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
