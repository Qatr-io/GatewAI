package metrics

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"
)

func newTestRedisForMetrics(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	return rdb, mr
}

func TestRefreshUsageTopN_PopulatesGauge(t *testing.T) {
	rdb, mr := newTestRedisForMetrics(t)
	mr.ZAdd("usage:consumer:transcription:tokens:prompt", 500, "alice")
	mr.ZAdd("usage:consumer:transcription:tokens:prompt", 200, "bob")
	mr.ZAdd("usage:consumer:transcription:tokens:completion", 80, "alice")

	refreshUsageTopN(context.Background(), rdb, 10, []string{"transcription"})

	if got := testutil.ToFloat64(UsageTokensTop.WithLabelValues("alice", "transcription", "prompt")); got != 500 {
		t.Errorf("alice prompt: got %v, want 500", got)
	}
	if got := testutil.ToFloat64(UsageTokensTop.WithLabelValues("bob", "transcription", "prompt")); got != 200 {
		t.Errorf("bob prompt: got %v, want 200", got)
	}
	if got := testutil.ToFloat64(UsageTokensTop.WithLabelValues("alice", "transcription", "completion")); got != 80 {
		t.Errorf("alice completion: got %v, want 80", got)
	}
}

func TestRefreshUsageTopN_PopulatesRequestsGauge(t *testing.T) {
	rdb, mr := newTestRedisForMetrics(t)
	mr.ZAdd("usage:consumer:llm:requests", 300, "alice")
	mr.ZAdd("usage:consumer:llm:requests", 150, "bob")

	refreshUsageTopN(context.Background(), rdb, 10, []string{"llm"})

	if got := testutil.ToFloat64(UsageRequestsTop.WithLabelValues("alice", "llm")); got != 300 {
		t.Errorf("alice requests: got %v, want 300", got)
	}
	if got := testutil.ToFloat64(UsageRequestsTop.WithLabelValues("bob", "llm")); got != 150 {
		t.Errorf("bob requests: got %v, want 150", got)
	}
}

func TestRefreshUsageTopN_PopulatesProcessingTimeGauge(t *testing.T) {
	rdb, mr := newTestRedisForMetrics(t)
	mr.ZAdd("usage:consumer:transcription:processing_time", 120, "alice")
	mr.ZAdd("usage:consumer:transcription:processing_time", 45, "bob")

	refreshUsageTopN(context.Background(), rdb, 10, []string{"transcription"})

	if got := testutil.ToFloat64(UsageProcessingTimeTop.WithLabelValues("alice", "transcription")); got != 120 {
		t.Errorf("alice processing_time: got %v, want 120", got)
	}
	if got := testutil.ToFloat64(UsageProcessingTimeTop.WithLabelValues("bob", "transcription")); got != 45 {
		t.Errorf("bob processing_time: got %v, want 45", got)
	}
}

func TestStartUsageTopNRefresh_TopNZero_NoOp(t *testing.T) {
	rdb, _ := newTestRedisForMetrics(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	StartUsageTopNRefresh(ctx, rdb, 0, time.Second, []string{"transcription"})
}

func TestStartUsageTopNRefresh_NoServiceTypes_NoOp(t *testing.T) {
	rdb, _ := newTestRedisForMetrics(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	StartUsageTopNRefresh(ctx, rdb, 10, time.Second, nil)
}
