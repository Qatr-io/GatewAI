package storage_test

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"

	"gatewai/gateway/internal/config"
	"gatewai/gateway/internal/storage"
)

func newTestRedisClient(t *testing.T) (*storage.RedisClient, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	c, err := storage.NewRedis(config.RedisConfig{Addr: mr.Addr()}, config.LifecycleConfig{})
	if err != nil {
		t.Fatalf("failed to create redis client: %v", err)
	}
	return c, mr
}

func TestJobsExistBatch(t *testing.T) {
	c, mr := newTestRedisClient(t)
	ctx := context.Background()

	mr.Set("job:abc", `{"id":"abc"}`)

	got, err := c.JobsExistBatch(ctx, []string{"abc", "def"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got["abc"] {
		t.Error("expected job abc to exist in Redis")
	}
	if got["def"] {
		t.Error("expected job def to be absent from Redis")
	}
}

func TestJobsExistBatch_EmptyInput(t *testing.T) {
	c, _ := newTestRedisClient(t)
	got, err := c.JobsExistBatch(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error on empty input: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty map, got %v", got)
	}
}

func TestJobsExistBatch_RedisError(t *testing.T) {
	c, mr := newTestRedisClient(t)
	mr.Close()

	_, err := c.JobsExistBatch(context.Background(), []string{"abc"})
	if err == nil {
		t.Error("expected error when Redis is unavailable")
	}
}

func TestSweepOrphanedRelayQueueEntries(t *testing.T) {
	c, mr := newTestRedisClient(t)
	ctx := context.Background()

	// live-job stays in processing (job record present, relay still working).
	mr.Set("job:live-job", `{"id":"live-job"}`)
	mr.Lpush("relay:whisper:processing", "live-job")

	// orphan-job's record has expired (e.g. relay crashed before calling Done).
	mr.Lpush("relay:whisper:processing", "orphan-job")

	// stale-pending-job was marked failed by phase 1 but never LRemmed from pending.
	mr.Lpush("relay:whisper:pending", "stale-pending-job")

	// unrelated-model queue must be untouched when not passed in models.
	mr.Lpush("relay:other:processing", "other-orphan")

	removed, err := c.SweepOrphanedRelayQueueEntries(ctx, []string{"whisper"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := map[string]int{}
	for _, r := range removed {
		got[r.Model+":"+r.State] = r.Count
	}
	if got["whisper:processing"] != 1 {
		t.Errorf("expected 1 orphan removed from whisper:processing, got %v", got)
	}
	if got["whisper:pending"] != 1 {
		t.Errorf("expected 1 orphan removed from whisper:pending, got %v", got)
	}

	processing, _ := mr.List("relay:whisper:processing")
	if len(processing) != 1 || processing[0] != "live-job" {
		t.Errorf("expected only live-job left in processing, got %v", processing)
	}
	pending, _ := mr.List("relay:whisper:pending")
	if len(pending) != 0 {
		t.Errorf("expected pending list empty, got %v", pending)
	}
	other, _ := mr.List("relay:other:processing")
	if len(other) != 1 {
		t.Errorf("expected untouched other-model queue, got %v", other)
	}
}

func TestSweepOrphanedRelayQueueEntries_EmptyQueues(t *testing.T) {
	c, _ := newTestRedisClient(t)
	removed, err := c.SweepOrphanedRelayQueueEntries(context.Background(), []string{"whisper"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(removed) != 0 {
		t.Errorf("expected no results for empty queues, got %v", removed)
	}
}

func TestSweepOrphanedRelayQueueEntries_RedisError(t *testing.T) {
	c, mr := newTestRedisClient(t)
	mr.Close()

	_, err := c.SweepOrphanedRelayQueueEntries(context.Background(), []string{"whisper"})
	if err == nil {
		t.Error("expected error when Redis is unavailable")
	}
}

func TestPing(t *testing.T) {
	c, _ := newTestRedisClient(t)
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("unexpected ping error: %v", err)
	}
}

func TestPing_Unavailable(t *testing.T) {
	c, mr := newTestRedisClient(t)
	mr.Close()
	if err := c.Ping(context.Background()); err == nil {
		t.Error("expected ping error when Redis is down")
	}
}
