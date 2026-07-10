package consumer

import (
	"context"
	"log/slog"
	"sync"

	"gatewai/gateway/internal/ratelimit"
	"gatewai/gateway/internal/service"
	"gatewai/gateway/internal/storage"
	"gatewai/gateway/internal/usage"
)

// Manager subscribes to job completion events for all async models.
// One goroutine per model listens on jobs:{model}:completed.
// On each completion: GetJob → NotifyJobDone → sendWebhook (if callback_url).
type Manager struct {
	redis         *storage.RedisClient
	webhookSender *WebhookSender
	sub           *Subscriber

	processingTimeLimiter ratelimit.ProcessingTimeChecker // nil = disabled
	tokenLimiter          ratelimit.TokenChecker          // nil = disabled
	usageTracker          usage.UsageTracker               // nil = no usage tracking

	mu        sync.Mutex
	parentCtx context.Context
	cancels   map[string]context.CancelFunc // keyed by model name
	wg        sync.WaitGroup                // tracks in-flight webhook goroutines
}

// NewManager creates a Manager that subscribes to Redis pub/sub job completion
// channels and dispatches webhook notifications.
func NewManager(redis *storage.RedisClient, s3 S3Store, persistsResult bool) *Manager {
	ws := NewWebhookSender(redis, s3, persistsResult)
	m := &Manager{
		redis:         redis,
		webhookSender: ws,
		cancels:       make(map[string]context.CancelFunc),
	}
	m.sub = NewSubscriber(redis.Client(), m.onComplete)
	return m
}

// Start subscribes to completion events for all models in the registry.
// Models without a Model field are skipped. Deduplicates by model name.
// All goroutines stop when ctx is cancelled; call Wait to drain webhook goroutines.
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

// WithProcessingTimeLimiter attaches a processing-time budget limiter. Call before Start.
func (m *Manager) WithProcessingTimeLimiter(l ratelimit.ProcessingTimeChecker) *Manager {
	m.processingTimeLimiter = l
	return m
}

// WithTokenLimiter attaches a token budget limiter. Call before Start.
func (m *Manager) WithTokenLimiter(l ratelimit.TokenChecker) *Manager {
	m.tokenLimiter = l
	return m
}

// WithUsageTracker attaches a usage tracker for processing-time recording.
func (m *Manager) WithUsageTracker(t usage.UsageTracker) *Manager {
	m.usageTracker = t
	return m
}

// UpdatePersistsResult updates S3 result retention policy.
// Safe to call concurrently with in-flight webhook goroutines.
func (m *Manager) UpdatePersistsResult(v bool) {
	m.webhookSender.UpdatePersistsResult(v)
}

// Wait drains all in-flight webhook goroutines.
func (m *Manager) Wait() {
	m.wg.Wait()
}

// startLocked starts a subscriber goroutine for model. Must be called with m.mu held.
func (m *Manager) startLocked(model string) {
	ctx, cancel := context.WithCancel(m.parentCtx)
	m.cancels[model] = cancel
	m.sub.Subscribe(ctx, model)
}

// onComplete is called by the Subscriber goroutine for each completed job.
// It fetches the job from Redis, notifies sync waiters, and dispatches webhooks.
func (m *Manager) onComplete(ctx context.Context, jobID string) {
	job, err := m.redis.GetJob(ctx, jobID)
	if err != nil {
		slog.Error("manager: failed to fetch job", "job_id", jobID, "error", err)
		return
	}

	m.redis.NotifyJobDone(ctx, jobID)

	if m.processingTimeLimiter != nil && job.ProcessingTime > 0 {
		if err := m.processingTimeLimiter.AddProcessingTime(ctx, job.ConsumerName, job.UserType, job.ServiceType, job.ProcessingTime); err != nil {
			slog.Error("manager: failed to add processing time", "job_id", jobID, "error", err)
		}
	}

	totalTokens := job.PromptTokens + job.CompletionTokens
	if m.tokenLimiter != nil && totalTokens > 0 {
		if err := m.tokenLimiter.AddTokensFor(ctx, job.ConsumerName, job.UserType, job.ServiceType, int(totalTokens)); err != nil {
			slog.Error("manager: failed to add tokens", "job_id", jobID, "error", err)
		}
	}

	if m.usageTracker != nil && job.ConsumerName != "" && job.ProcessingTime > 0 {
		m.usageTracker.TrackProcessingTime(ctx, job.ConsumerName, job.ServiceType, job.ProcessingTime)
		m.usageTracker.TrackActive(ctx, job.ConsumerName)
	}

	if m.usageTracker != nil && job.ConsumerName != "" && totalTokens > 0 {
		m.usageTracker.TrackTokens(ctx, job.ConsumerName, job.ServiceType, job.PromptTokens, job.CompletionTokens)
	}

	if job.CallbackURL != "" {
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			m.webhookSender.Send(job)
		}()
	}
}
