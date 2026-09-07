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

	// wg tracks in-flight webhook + result-scan goroutines. lifeMu/closing
	// serialize wg.Add against Wait so a completion trigger arriving during
	// shutdown can never Add to a zero counter concurrently with Wait (which is
	// an illegal WaitGroup use). ProcessCompletion is now driven from two
	// sources — the pub/sub subscriber goroutine and the HTTP handler — so this
	// guard is required, not merely defensive.
	lifeMu  sync.RWMutex
	closing bool
	wg      sync.WaitGroup
}

// trackStart reserves a slot on the in-flight WaitGroup unless the handler is
// shutting down. Returns false (do not start the goroutine) once Wait has been
// called. Callers that get true MUST arrange a matching wg.Done.
func (h *RelayCompleteHandler) trackStart() bool {
	h.lifeMu.RLock()
	defer h.lifeMu.RUnlock()
	if h.closing {
		return false
	}
	h.wg.Add(1)
	return true
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

// Wait stops accepting new completion work and drains all in-flight webhook +
// result-scan goroutines. Call during graceful shutdown. After Wait, further
// ProcessCompletion calls are no-ops (their work is bounded by the async gate
// timeout on the poll path). Latching closing under the write lock guarantees no
// wg.Add races the wg.Wait below.
func (h *RelayCompleteHandler) Wait() {
	h.lifeMu.Lock()
	h.closing = true
	h.lifeMu.Unlock()
	h.wg.Wait()
}

// Complete handles POST /-/relay/jobs/{id}/complete. This is a redundant
// fast-path trigger for the completion work: the reliable driver is the Redis
// pub/sub broadcast (consumer.Manager.onComplete), which every replica receives.
// This HTTP call lets the work fire promptly and still run if a broadcast is
// missed; the exactly-once claim inside ProcessCompletion dedupes the two.
func (h *RelayCompleteHandler) Complete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if _, err := h.redis.GetJob(r.Context(), id); err != nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("job %q not found", id))
		return
	}
	w.WriteHeader(http.StatusOK)
	h.ProcessCompletion(context.WithoutCancel(r.Context()), id)
}

// ProcessCompletion runs the exactly-once completion work for a job. It is
// invoked from BOTH triggers — the pub/sub broadcast (on every replica) and the
// /complete HTTP fast-path — and claims the job in Redis so exactly one caller
// performs the work regardless of how many triggers fire. The heavy work runs
// in a tracked background goroutine so neither trigger blocks. A no-op for the
// callers that lose the claim.
func (h *RelayCompleteHandler) ProcessCompletion(ctx context.Context, jobID string) {
	won, err := h.redis.ClaimCompletion(ctx, jobID)
	if err != nil {
		// Redis unreachable: skip rather than risk N replicas all running the work
		// undeduped. A missed completion is bounded by the async gate's timeout.
		slog.WarnContext(ctx, "completion claim failed", "job_id", jobID, "error", err)
		return
	}
	if !won {
		return // another trigger already owns this job's completion work
	}
	job, err := h.redis.GetJob(ctx, jobID)
	if err != nil || job == nil {
		slog.WarnContext(ctx, "completion: job not found after claim", "job_id", jobID, "error", err)
		return
	}
	if !h.trackStart() {
		return // shutting down; the async gate timeout bounds the un-run work
	}
	go func() {
		defer h.wg.Done()
		h.runCompletionWork(context.WithoutCancel(ctx), job)
	}()
}

// runCompletionWork performs the once-per-job completion work: rate-limit and
// token debit, usage tracking, result-stage (async) guardrails, and webhook
// delivery. Callers must have won the completion claim first.
func (h *RelayCompleteHandler) runCompletionWork(ctx context.Context, job *model.Job) {
	if h.processingTimeLimiter != nil && job.ProcessingTime > 0 {
		if err := h.processingTimeLimiter.AddProcessingTime(ctx, job.ConsumerName, job.UserType, job.ServiceType, job.ProcessingTime); err != nil {
			slog.ErrorContext(ctx, "relaycomplete: failed to add processing time", "job_id", job.ID, "error", err)
		}
	}

	totalTokens := job.PromptTokens + job.CompletionTokens
	if h.tokenLimiter != nil && totalTokens > 0 {
		if err := h.tokenLimiter.AddTokensFor(ctx, job.ConsumerName, job.UserType, job.ServiceType, int(totalTokens)); err != nil {
			slog.ErrorContext(ctx, "relaycomplete: failed to add tokens", "job_id", job.ID, "error", err)
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

	// Result-stage (async) guardrails run before webhook delivery so a blocked
	// job is failed and a redacted result is repointed before the client is
	// notified. When no async guardrails apply this is exactly the previous
	// behaviour (deliver the webhook).
	def := h.asyncGuardrailsFor(job)
	if def != nil {
		// Enforcing async guardrails gate the client-facing result. The gate is
		// armed at submit (so a lost trigger cannot leave the result ungated), with
		// the effective deadline derived from the completion time on the poll path.
		// Here we run the scan, then clear the gate and wake sync waiters so the
		// poll path delivers only the scanned (block/redact) outcome.
		gated := def.Guardrails.Async.Enforcing()
		h.applyResultGuardrails(ctx, def, job)
		if gated {
			if err := h.redis.ClearScanGate(ctx, job.ID); err != nil {
				slog.WarnContext(ctx, "async guardrails: failed to clear scan gate", "job_id", job.ID, "error", err)
			}
			h.redis.NotifyJobDone(ctx, job.ID)
		}
	}
	if job.CallbackURL != "" {
		h.webhookSender.Send(job)
	}
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
