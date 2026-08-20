package storage_test

import (
	"context"
	"testing"

	"gatewai/gateway/internal/model"
)

// seedProcessing puts a job record + a processing-list entry in place, with no
// lease (simulating a relay pod that died mid-job).
func seedProcessing(t *testing.T, mr interface {
	Set(string, string) error
	RPush(string, ...string) (int, error)
}, model, id, status string) {
	t.Helper()
	if err := mr.Set("job:"+id, `{"id":"`+id+`","model":"`+model+`","status":"`+status+`"}`); err != nil {
		t.Fatalf("seed job %s: %v", id, err)
	}
	if _, err := mr.RPush("relay:"+model+":processing", id); err != nil {
		t.Fatalf("seed processing %s: %v", id, err)
	}
}

func TestReap_RequeuesOrphanedJob(t *testing.T) {
	c, mr := newTestRedisClient(t)
	ctx := context.Background()
	seedProcessing(t, mr, "m", "j1", "processing") // no lease → orphaned

	res, err := c.ReapOrphanedProcessingJobs(ctx, 3)
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if res.Requeued != 1 {
		t.Fatalf("expected 1 requeued, got %+v", res)
	}
	if items, _ := mr.List("relay:m:pending"); len(items) != 1 || items[0] != "j1" {
		t.Fatalf("expected j1 requeued to pending, got %v", items)
	}
	if items, _ := mr.List("relay:m:processing"); len(items) != 0 {
		t.Fatalf("expected processing empty, got %v", items)
	}
	job, err := c.GetJob(ctx, "j1")
	if err != nil || job.Status != model.JobStatusPending {
		t.Fatalf("expected job reset to pending, got %v (err=%v)", job, err)
	}
}

func TestReap_SkipsJobWithLiveLease(t *testing.T) {
	c, mr := newTestRedisClient(t)
	ctx := context.Background()
	seedProcessing(t, mr, "m", "j2", "processing")
	_ = mr.Set("relay:m:lease:j2", "worker-a") // live lease

	res, err := c.ReapOrphanedProcessingJobs(ctx, 3)
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if res.Requeued != 0 || res.DeadLettered != 0 || res.Dropped != 0 {
		t.Fatalf("expected no action for leased job, got %+v", res)
	}
	if items, _ := mr.List("relay:m:processing"); len(items) != 1 {
		t.Fatalf("expected leased job to stay in processing, got %v", items)
	}
}

func TestReap_DeadLettersAfterMaxAttempts(t *testing.T) {
	c, mr := newTestRedisClient(t)
	ctx := context.Background()
	seedProcessing(t, mr, "m", "j3", "processing")
	_ = mr.Set("relay:m:attempts:j3", "3") // already at cap; INCR → 4 > 3

	res, err := c.ReapOrphanedProcessingJobs(ctx, 3)
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if res.DeadLettered != 1 {
		t.Fatalf("expected 1 dead-lettered, got %+v", res)
	}
	if items, _ := mr.List("relay:m:deadletter"); len(items) != 1 || items[0] != "j3" {
		t.Fatalf("expected j3 in dead-letter list, got %v", items)
	}
	if items, _ := mr.List("relay:m:pending"); len(items) != 0 {
		t.Fatalf("expected no requeue after dead-letter, got %v", items)
	}
	job, err := c.GetJob(ctx, "j3")
	if err != nil || job.Status != model.JobStatusFailed {
		t.Fatalf("expected job marked failed, got %v (err=%v)", job, err)
	}
}

func TestReap_DropsWhenRecordMissing(t *testing.T) {
	c, mr := newTestRedisClient(t)
	ctx := context.Background()
	// Processing entry with no job record (TTL expired) and no lease.
	if _, err := mr.RPush("relay:m:processing", "j4"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	res, err := c.ReapOrphanedProcessingJobs(ctx, 3)
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if res.Dropped != 1 {
		t.Fatalf("expected 1 dropped, got %+v", res)
	}
	if items, _ := mr.List("relay:m:processing"); len(items) != 0 {
		t.Fatalf("expected processing cleaned up, got %v", items)
	}
	if items, _ := mr.List("relay:m:pending"); len(items) != 0 {
		t.Fatalf("expected no requeue for missing record, got %v", items)
	}
}
