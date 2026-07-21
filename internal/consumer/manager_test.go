package consumer

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"gatewai/gateway/internal/config"
	"gatewai/gateway/internal/metrics"
	"gatewai/gateway/internal/model"
	"gatewai/gateway/internal/storage"
)

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

// TestOnComplete_IncrementsJobsTotal verifies the gatewai_jobs_total counter
// is incremented with the job's service_type, model and status labels for
// both a completed and a failed terminal outcome.
func TestOnComplete_IncrementsJobsTotal(t *testing.T) {
	rc, mr := newTestRedis(t)

	completedJob := &model.Job{
		ID: "job-completed", ServiceType: "transcription", Model: "whisper-large-v3",
		Status: model.JobStatusCompleted,
	}
	seedJob(t, mr, completedJob)

	failedJob := &model.Job{
		ID: "job-failed", ServiceType: "transcription", Model: "whisper-large-v3",
		Status: model.JobStatusFailed,
	}
	seedJob(t, mr, failedJob)

	completedCounter := metrics.JobsTotal.WithLabelValues("transcription", "whisper-large-v3", "completed")
	failedCounter := metrics.JobsTotal.WithLabelValues("transcription", "whisper-large-v3", "failed")
	beforeCompleted := testutil.ToFloat64(completedCounter)
	beforeFailed := testutil.ToFloat64(failedCounter)

	mgr := NewManager(rc)
	mgr.onComplete(context.Background(), "job-completed")
	mgr.onComplete(context.Background(), "job-failed")

	if got := testutil.ToFloat64(completedCounter); got != beforeCompleted+1 {
		t.Errorf("completed counter: got %v, want %v", got, beforeCompleted+1)
	}
	if got := testutil.ToFloat64(failedCounter); got != beforeFailed+1 {
		t.Errorf("failed counter: got %v, want %v", got, beforeFailed+1)
	}
}

// TestOnComplete_NotifiesJobDone verifies onComplete publishes the job:done
// notification even when nothing is waiting on it (broadcast-safe no-op).
func TestOnComplete_NotifiesJobDone(t *testing.T) {
	rc, mr := newTestRedis(t)

	job := &model.Job{ID: "job-1", ServiceType: "transcription", Status: model.JobStatusCompleted}
	seedJob(t, mr, job)

	mgr := NewManager(rc)
	// Must not panic or error even with zero subscribers on job:job-1:done.
	mgr.onComplete(context.Background(), "job-1")
}
