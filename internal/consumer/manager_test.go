package consumer

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/alicebob/miniredis/v2"

	"gatewai/gateway/internal/config"
	"gatewai/gateway/internal/model"
	"gatewai/gateway/internal/ratelimit"
	"gatewai/gateway/internal/storage"
)

// ── test doubles ─────────────────────────────────────────────────────────────

type mockPTLimiter struct {
	addCalls []struct {
		consumer, userType, svcType string
		seconds                     float64
	}
}

func (m *mockPTLimiter) CheckProcessingTime(_ context.Context, _ *http.Request, _ string) (ratelimit.CheckResult, error) {
	return ratelimit.CheckResult{Allowed: true}, nil
}

func (m *mockPTLimiter) AddProcessingTime(_ context.Context, consumer, userType, svcType string, seconds float64) error {
	m.addCalls = append(m.addCalls, struct {
		consumer, userType, svcType string
		seconds                     float64
	}{consumer, userType, svcType, seconds})
	return nil
}

type mockTokenLimiterForManager struct {
	calls []struct {
		consumer, userType, serviceType string
		total                            int
	}
}

func (m *mockTokenLimiterForManager) CheckTokens(_ context.Context, _ *http.Request, _ string) (ratelimit.CheckResult, error) {
	return ratelimit.CheckResult{Allowed: true}, nil
}
func (m *mockTokenLimiterForManager) AddTokens(_ context.Context, _ *http.Request, _ string, _ int) error {
	return nil
}
func (m *mockTokenLimiterForManager) CheckModelTokens(_ context.Context, _ *http.Request, _ string) (ratelimit.CheckResult, error) {
	return ratelimit.CheckResult{Allowed: true}, nil
}
func (m *mockTokenLimiterForManager) AddModelTokens(_ context.Context, _ *http.Request, _ string, _ int) error {
	return nil
}
func (m *mockTokenLimiterForManager) AddTokensFor(_ context.Context, consumer, userType, serviceType string, total int) error {
	m.calls = append(m.calls, struct {
		consumer, userType, serviceType string
		total                            int
	}{consumer, userType, serviceType, total})
	return nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

func newTestRedis(t *testing.T) (*storage.RedisClient, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	c, err := storage.NewRedis(config.RedisConfig{Addr: mr.Addr()}, config.LifecycleConfig{})
	if err != nil {
		t.Fatalf("failed to create redis client: %v", err)
	}
	return c, mr
}

func seedJob(t *testing.T, mr *miniredis.Miniredis, job *model.Job) {
	t.Helper()
	data, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("marshal job: %v", err)
	}
	mr.Set("job:"+job.ID, string(data))
}

// ── tests ─────────────────────────────────────────────────────────────────────

// TestOnComplete_CallsAddProcessingTime verifies AddProcessingTime is called
// with the job's processing_time when it is non-zero.
func TestOnComplete_CallsAddProcessingTime(t *testing.T) {
	rc, mr := newTestRedis(t)

	job := &model.Job{
		ID:             "j2",
		ServiceType:    "transcription",
		ConsumerName:   "consumer-b",
		UserType:       "basic",
		Status:         model.JobStatusCompleted,
		ProcessingTime: 9.71,
	}
	seedJob(t, mr, job)

	pt := &mockPTLimiter{}
	mgr := NewManager(rc, nil, false).WithProcessingTimeLimiter(pt)
	mgr.onComplete(context.Background(), "j2")

	if len(pt.addCalls) != 1 {
		t.Fatalf("expected 1 AddProcessingTime call, got %d", len(pt.addCalls))
	}
	a := pt.addCalls[0]
	if a.consumer != "consumer-b" || a.userType != "basic" || a.svcType != "transcription" || a.seconds != 9.71 {
		t.Errorf("unexpected AddProcessingTime args: %+v", a)
	}
}

// TestOnComplete_SkipsAddProcessingTime_WhenZero verifies that AddProcessingTime
// is not called when processing_time is 0 (not set by relay).
func TestOnComplete_SkipsAddProcessingTime_WhenZero(t *testing.T) {
	rc, mr := newTestRedis(t)

	job := &model.Job{
		ID:             "j3",
		ServiceType:    "transcription",
		Status:         model.JobStatusCompleted,
		ProcessingTime: 0,
	}
	seedJob(t, mr, job)

	pt := &mockPTLimiter{}
	mgr := NewManager(rc, nil, false).WithProcessingTimeLimiter(pt)
	mgr.onComplete(context.Background(), "j3")

	if len(pt.addCalls) != 0 {
		t.Errorf("expected no AddProcessingTime call for zero processing_time, got %d", len(pt.addCalls))
	}
}

// TestOnComplete_CallsAddTokensFor verifies AddTokensFor is called with the
// job's combined prompt+completion tokens when non-zero.
func TestOnComplete_CallsAddTokensFor(t *testing.T) {
	rc, mr := newTestRedis(t)

	job := &model.Job{
		ID: "job-1", ServiceType: "transcription", ConsumerName: "alice", UserType: "user",
		Status: model.JobStatusCompleted, PromptTokens: 100, CompletionTokens: 20,
	}
	seedJob(t, mr, job)

	tl := &mockTokenLimiterForManager{}
	mgr := NewManager(rc, nil, false).WithTokenLimiter(tl)
	mgr.onComplete(context.Background(), "job-1")

	if len(tl.calls) != 1 {
		t.Fatalf("expected 1 AddTokensFor call, got %d", len(tl.calls))
	}
	if tl.calls[0].total != 120 {
		t.Errorf("total: got %d, want 120", tl.calls[0].total)
	}
	if tl.calls[0].consumer != "alice" {
		t.Errorf("consumer: got %q, want alice", tl.calls[0].consumer)
	}
}

// TestOnComplete_SkipsAddTokensFor_WhenZero verifies AddTokensFor is not
// called when the job has no token data (not reported by the relay).
func TestOnComplete_SkipsAddTokensFor_WhenZero(t *testing.T) {
	rc, mr := newTestRedis(t)

	job := &model.Job{
		ID: "job-2", ServiceType: "transcription", ConsumerName: "alice", UserType: "user",
		Status: model.JobStatusCompleted,
	}
	seedJob(t, mr, job)

	tl := &mockTokenLimiterForManager{}
	mgr := NewManager(rc, nil, false).WithTokenLimiter(tl)
	mgr.onComplete(context.Background(), "job-2")

	if len(tl.calls) != 0 {
		t.Errorf("expected no AddTokensFor call for zero tokens, got %d", len(tl.calls))
	}
}
