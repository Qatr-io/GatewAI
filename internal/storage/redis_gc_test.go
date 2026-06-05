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
