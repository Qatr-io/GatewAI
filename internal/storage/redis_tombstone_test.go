package storage_test

import (
	"context"
	"testing"
	"time"

	"gatewai/gateway/internal/model"
)

// TestJobEverExisted verifies SaveJob writes a job-existence tombstone that
// outlives the record and that JobEverExisted reads it.
func TestJobEverExisted(t *testing.T) {
	c, mr := newTestRedisClient(t)
	ctx := context.Background()

	job := &model.Job{ID: "j1", ServiceType: "audio", Model: "whisper", Status: model.JobStatusPending, CreatedAt: time.Now()}
	if err := c.SaveJob(ctx, job); err != nil {
		t.Fatalf("SaveJob: %v", err)
	}

	if ok, err := c.JobEverExisted(ctx, "j1"); err != nil || !ok {
		t.Fatalf("expected tombstone present for j1, got ok=%v err=%v", ok, err)
	}
	if !mr.Exists("jobmeta:j1") {
		t.Fatal("expected jobmeta:j1 key to exist")
	}
	if ok, _ := c.JobEverExisted(ctx, "never"); ok {
		t.Fatal("expected no tombstone for an unknown id")
	}

	// The tombstone must outlive the job record: delete the record, tombstone stays.
	mr.Del("job:j1")
	if ok, _ := c.JobEverExisted(ctx, "j1"); !ok {
		t.Fatal("tombstone should survive the job record being gone")
	}
}
