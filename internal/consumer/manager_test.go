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
