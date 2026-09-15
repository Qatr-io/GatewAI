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
	mr.HSet("usage:consumer:transcription:usertype", "alice", "sa")
	mr.HSet("usage:consumer:transcription:usertype", "bob", "user")

	refreshUsageTopN(context.Background(), rdb, 10, []string{"transcription"})

	if got := testutil.ToFloat64(UsageTokensTop.WithLabelValues("alice", "transcription", "prompt", "sa")); got != 500 {
		t.Errorf("alice prompt: got %v, want 500", got)
	}
	if got := testutil.ToFloat64(UsageTokensTop.WithLabelValues("bob", "transcription", "prompt", "user")); got != 200 {
		t.Errorf("bob prompt: got %v, want 200", got)
	}
	if got := testutil.ToFloat64(UsageTokensTop.WithLabelValues("alice", "transcription", "completion", "sa")); got != 80 {
		t.Errorf("alice completion: got %v, want 80", got)
	}
}

func TestRefreshUsageTopN_PopulatesRequestsGauge(t *testing.T) {
	rdb, mr := newTestRedisForMetrics(t)
	mr.ZAdd("usage:consumer:llm:requests", 300, "alice")
	mr.ZAdd("usage:consumer:llm:requests", 150, "bob")
	mr.HSet("usage:consumer:llm:usertype", "alice", "sa")
	mr.HSet("usage:consumer:llm:usertype", "bob", "user")

	refreshUsageTopN(context.Background(), rdb, 10, []string{"llm"})

	if got := testutil.ToFloat64(UsageRequestsTop.WithLabelValues("alice", "llm", "sa")); got != 300 {
		t.Errorf("alice requests: got %v, want 300", got)
	}
	if got := testutil.ToFloat64(UsageRequestsTop.WithLabelValues("bob", "llm", "user")); got != 150 {
		t.Errorf("bob requests: got %v, want 150", got)
	}
}

func TestRefreshUsageTopN_PopulatesProcessingTimeGauge(t *testing.T) {
	rdb, mr := newTestRedisForMetrics(t)
	mr.ZAdd("usage:consumer:transcription:processing_time", 120, "alice")
	mr.ZAdd("usage:consumer:transcription:processing_time", 45, "bob")
	mr.HSet("usage:consumer:transcription:usertype", "alice", "sa")
	mr.HSet("usage:consumer:transcription:usertype", "bob", "user")

	refreshUsageTopN(context.Background(), rdb, 10, []string{"transcription"})

	if got := testutil.ToFloat64(UsageProcessingTimeTop.WithLabelValues("alice", "transcription", "sa")); got != 120 {
		t.Errorf("alice processing_time: got %v, want 120", got)
	}
	if got := testutil.ToFloat64(UsageProcessingTimeTop.WithLabelValues("bob", "transcription", "user")); got != 45 {
		t.Errorf("bob processing_time: got %v, want 45", got)
	}
}

func TestRefreshUsageTopN_MissingUserTypeIsEmpty(t *testing.T) {
	rdb, mr := newTestRedisForMetrics(t)
	mr.ZAdd("usage:consumer:transcription:requests", 10, "carol")
	// No usertype hash entry for carol: TrackUserType has never fired for her.

	refreshUsageTopN(context.Background(), rdb, 10, []string{"transcription"})

	if got := testutil.ToFloat64(UsageRequestsTop.WithLabelValues("carol", "transcription", "")); got != 10 {
		t.Errorf("carol requests: got %v, want 10", got)
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
