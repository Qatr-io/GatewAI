package main

import (
	"context"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"gatewai/gateway/internal/config"
	"gatewai/gateway/internal/metrics"
	"gatewai/gateway/internal/service"
	"gatewai/gateway/internal/storage"
)

func newTestRegistry(t *testing.T) *service.Registry {
	t.Helper()
	return service.NewRegistry([]config.ServiceConfig{{
		Type:          "transcription",
		Model:         "whisper-large-v3",
		AcceptedExts:  []string{".mp3"},
		MaxFileSizeMB: 100,
	}})
}

func newTestRedisClient(t *testing.T) *storage.RedisClient {
	t.Helper()
	mr := miniredis.RunT(t)
	c, err := storage.NewRedis(config.RedisConfig{Addr: mr.Addr()}, config.LifecycleConfig{})
	if err != nil {
		t.Fatalf("failed to create redis client: %v", err)
	}
	return c
}

// TestRunOrphanRelayQueueCleanup_RemovesEntryAndRecordsMetric verifies the GC
// phase 3 wiring: a job stuck in relay:{model}:processing whose job record no
// longer exists in Redis (e.g. the relay pod crashed before calling Done, or
// its TTL expired) is removed from the list and counted in
// gatewai_relay_queue_orphans_swept_total.
func TestRunOrphanRelayQueueCleanup_RemovesEntryAndRecordsMetric(t *testing.T) {
	metrics.RelayQueueOrphansSweptTotal.Reset()

	c := newTestRedisClient(t)
	ctx := context.Background()

	if err := c.Raw().LPush(ctx, "relay:whisper-large-v3:processing", "orphan-job").Err(); err != nil {
		t.Fatalf("failed to seed processing list: %v", err)
	}

	runOrphanRelayQueueCleanup(ctx, c, newTestRegistry(t))

	n, err := c.Raw().LLen(ctx, "relay:whisper-large-v3:processing").Result()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 0 {
		t.Errorf("expected orphaned entry removed, processing list still has %d entries", n)
	}

	expected := `
# HELP gatewai_relay_queue_orphans_swept_total Total relay queue entries removed by the GC because their job record no longer exists in Redis.
# TYPE gatewai_relay_queue_orphans_swept_total counter
gatewai_relay_queue_orphans_swept_total{model="whisper-large-v3",state="processing"} 1
`
	if err := testutil.CollectAndCompare(metrics.RelayQueueOrphansSweptTotal, strings.NewReader(expected), "gatewai_relay_queue_orphans_swept_total"); err != nil {
		t.Errorf("unexpected metric output: %v", err)
	}
}

// TestRunOrphanRelayQueueCleanup_LeavesLiveJobAlone ensures an in-flight job
// (record still present in Redis) is never removed from the processing list.
func TestRunOrphanRelayQueueCleanup_LeavesLiveJobAlone(t *testing.T) {
	metrics.RelayQueueOrphansSweptTotal.Reset()

	c := newTestRedisClient(t)
	ctx := context.Background()

	if err := c.Raw().Set(ctx, "job:live-job", `{"id":"live-job"}`, 0).Err(); err != nil {
		t.Fatalf("failed to seed job record: %v", err)
	}
	if err := c.Raw().LPush(ctx, "relay:whisper-large-v3:processing", "live-job").Err(); err != nil {
		t.Fatalf("failed to seed processing list: %v", err)
	}

	runOrphanRelayQueueCleanup(ctx, c, newTestRegistry(t))

	n, err := c.Raw().LLen(ctx, "relay:whisper-large-v3:processing").Result()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 1 {
		t.Errorf("expected live job to stay in processing list, got %d entries", n)
	}
}

// TestRunOrphanRelayQueueCleanup_NilRegistry guards the reload race where the
// registry hasn't been set yet.
func TestRunOrphanRelayQueueCleanup_NilRegistry(t *testing.T) {
	c := newTestRedisClient(t)
	runOrphanRelayQueueCleanup(context.Background(), c, nil)
}
