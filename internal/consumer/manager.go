package consumer

import (
	"context"
	"log/slog"
	"sync"

	"gatewai/gateway/internal/metrics"
	"gatewai/gateway/internal/service"
	"gatewai/gateway/internal/storage"
)

// Manager subscribes to job completion events for all async models.
// One goroutine per model listens on jobs:{model}:completed.
// On each completion: GetJob → increment gatewai_jobs_total → NotifyJobDone.
//
// Every gateway replica receives every completion (Redis pub/sub is a
// broadcast, not a competing-consumer queue), so onComplete must only do work
// that is safe to run N times per job: the counter (each replica already
// holds the full correct count — see dashboards/gatewai.json's use of max())
// and NotifyJobDone (a harmless no-op on any replica with no matching sync
// waiter). Rate-limit debit, usage tracking, and webhook delivery are NOT
// safe to run N times and have moved to the relay, which processes each job
// exactly once (relay/internal/accounting, relay/internal/webhook).
type Manager struct {
	redis *storage.RedisClient
	sub   *Subscriber

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

	newModels := make(map[string]struct{})
	for _, def := range newReg.All() {
		if def.Model == "" {
			continue
		}
		newModels[def.Model] = struct{}{}
	}

	for model, cancel := range m.cancels {
		if _, ok := newModels[model]; !ok {
			slog.Info("stopping job completion subscriber", "model", model)
			cancel()
			delete(m.cancels, model)
		}
	}

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

	m.redis.NotifyJobDone(ctx, jobID)
}
