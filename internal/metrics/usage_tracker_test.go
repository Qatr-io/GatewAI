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
