package consumer

import (
	"context"
	"log/slog"
	"sync"

	"gatewai/gateway/internal/metrics"
	"gatewai/gateway/internal/service"
	"gatewai/gateway/internal/storage"
)

// CompletionProcessor runs the exactly-once completion work for a job — webhook
// delivery, result-stage guardrail scan, rate/token debit, usage tracking.
// Implemented by handler.RelayCompleteHandler. onComplete calls it on every
// replica; the implementation claims the job in Redis so exactly one replica
// performs the work (safe to invoke N times).
type CompletionProcessor interface {
	ProcessCompletion(ctx context.Context, jobID string)
}

// Manager subscribes to job completion events for all async models.
// One goroutine per model listens on jobs:{model}:completed.
//
// Every gateway replica receives every completion (Redis pub/sub is a
// broadcast, not a competing-consumer queue), so onComplete only does work that
// is safe to run N times per job: the counter (each replica already holds the
// full correct count — see dashboards/gatewai.json's use of max()) and
// NotifyJobDone (a harmless no-op on any replica with no matching sync waiter).
// The once-per-job work — rate-limit debit, usage tracking, result-stage
// guardrails, and webhook delivery — is delegated to a CompletionProcessor,
// which self-dedupes via a Redis claim. The broadcast is the RELIABLE driver of
// that work; the relay's targeted /complete HTTP call is a redundant fast-path
// onto the same claim, so a missed broadcast or a lost HTTP call alone never
// drops a webhook.
type Manager struct {
	redis     *storage.RedisClient
	sub       *Subscriber
	processor CompletionProcessor // nil = once-per-job work not wired

	mu        sync.Mutex
	parentCtx context.Context
	cancels   map[string]context.CancelFunc // keyed by model name
}

// NewManager creates a Manager that subscribes to Redis pub/sub job completion channels.
func NewManager(redis *storage.RedisClient) *Manager {
	m := &Manager{
		redis:   redis,
		cancels: make(map[string]context.CancelFunc),
	}
	m.sub = NewSubscriber(redis.Client(), m.onComplete)
	return m
}

// WithCompletionProcessor wires the exactly-once completion work (webhook,
// result scan, debits) so the pub/sub broadcast drives it. Call before Start.
func (m *Manager) WithCompletionProcessor(p CompletionProcessor) *Manager {
	m.processor = p
	return m
}

// Start subscribes to completion events for all models in the registry.
// Models without a Model field are skipped. Deduplicates by model name.
func (m *Manager) Start(ctx context.Context, reg *service.Registry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.parentCtx = ctx

	seen := make(map[string]struct{})
	for _, def := range reg.All() {
		if def.Model == "" {
			continue
		}
		if _, ok := seen[def.Model]; ok {
			continue
		}
		seen[def.Model] = struct{}{}
		m.startLocked(def.Model)
	}
}

// Reconcile stops subscriptions for removed models and starts new ones.
// Safe to call while the manager is running.
func (m *Manager) Reconcile(newReg *service.Registry) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Build the new model set (deduplicated).
	newModels := make(map[string]struct{})
	for _, def := range newReg.All() {
		if def.Model == "" {
			continue
		}
		newModels[def.Model] = struct{}{}
	}

	// Stop subscriptions for removed models.
	for model, cancel := range m.cancels {
		if _, ok := newModels[model]; !ok {
			slog.Info("stopping job completion subscriber", "model", model)
			cancel()
			delete(m.cancels, model)
		}
	}

	// Start subscriptions for new models.
	for model := range newModels {
		if _, ok := m.cancels[model]; !ok {
			slog.Info("starting job completion subscriber", "model", model)
			m.startLocked(model)
		}
	}
}

// startLocked starts a subscriber goroutine for model. Must be called with m.mu held.
func (m *Manager) startLocked(model string) {
	ctx, cancel := context.WithCancel(m.parentCtx)
	m.cancels[model] = cancel
	m.sub.Subscribe(ctx, model)
}

// onComplete is called by the Subscriber goroutine for each completed job, on
// EVERY gateway replica. Only perform work here that is safe to run N times.
func (m *Manager) onComplete(ctx context.Context, jobID string) {
	job, err := m.redis.GetJob(ctx, jobID)
	if err != nil {
		slog.Error("manager: failed to fetch job", "job_id", jobID, "error", err)
		return
	}

	metrics.JobsTotal.WithLabelValues(job.ServiceType, job.Model, string(job.Status)).Inc()

	// Drive the once-per-job completion work (webhook, result scan, debits) from
	// this reliable broadcast; the processor claims the job so only one replica
	// runs it. This is the primary trigger — the relay's /complete HTTP call is a
	// redundant fast-path onto the same claim.
	if m.processor != nil {
		m.processor.ProcessCompletion(ctx, jobID)
	}

	// For enforcing (block/redact) async guardrails a result DONE-gate is armed;
	// defer waking sync waiters until the completion work's scan clears it and
	// notifies (done by the claim winner). Non-gated jobs notify immediately.
	if _, gated, gerr := m.redis.GetScanGate(ctx, jobID); gerr == nil && gated {
		return
	}
	m.redis.NotifyJobDone(ctx, jobID)
}
