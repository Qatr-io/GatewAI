package usage_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"gatewai/gateway/internal/usage"
)

func newTracker(t *testing.T, retention time.Duration) (usage.UsageTracker, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return usage.NewRedisUsageTracker(rdb, retention), mr
}

func TestTrackRequest_IncrementsScore(t *testing.T) {
	tracker, mr := newTracker(t, 0)
	ctx := context.Background()

	tracker.TrackRequest(ctx, "alice", "audio")
	tracker.TrackRequest(ctx, "alice", "audio")
	tracker.TrackRequest(ctx, "bob", "audio")

	score, err := mr.ZScore("usage:consumer:audio:requests", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if score != 2 {
		t.Errorf("alice score = %v, want 2", score)
	}
	bobScore, _ := mr.ZScore("usage:consumer:audio:requests", "bob")
	if bobScore != 1 {
		t.Errorf("bob score = %v, want 1", bobScore)
	}
}

func TestTrackJob_IncrementsJobCounter(t *testing.T) {
	tracker, mr := newTracker(t, 0)
	tracker.TrackJob(context.Background(), "alice", "audio")
	tracker.TrackJob(context.Background(), "alice", "audio")

	score, err := mr.ZScore("usage:consumer:audio:jobs", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if score != 2 {
		t.Errorf("got %v, want 2", score)
	}
}

func TestTrackProcessingTime_AccumulatesSeconds(t *testing.T) {
	tracker, mr := newTracker(t, 0)
	tracker.TrackProcessingTime(context.Background(), "alice", "audio", 30.5)
	tracker.TrackProcessingTime(context.Background(), "alice", "audio", 10.0)

	score, _ := mr.ZScore("usage:consumer:audio:processing_time", "alice")
	if score != 40.5 {
		t.Errorf("got %v, want 40.5", score)
	}
}

func TestTrackTokens_AccumulatesPromptAndCompletion(t *testing.T) {
	tracker, mr := newTracker(t, 0)
	tracker.TrackTokens(context.Background(), "alice", "transcription", 100, 20)
	tracker.TrackTokens(context.Background(), "alice", "transcription", 50, 10)

	prompt, err := mr.ZScore("usage:consumer:transcription:tokens:prompt", "alice")
	if err != nil {
		t.Fatalf("ZScore prompt: %v", err)
	}
	if prompt != 150 {
		t.Errorf("prompt: got %v, want 150", prompt)
	}
	completion, err := mr.ZScore("usage:consumer:transcription:tokens:completion", "alice")
	if err != nil {
		t.Fatalf("ZScore completion: %v", err)
	}
	if completion != 30 {
		t.Errorf("completion: got %v, want 30", completion)
	}
}

func TestTrackTokens_ZeroTokens_DoesNothing(t *testing.T) {
	tracker, mr := newTracker(t, 0)
	tracker.TrackTokens(context.Background(), "alice", "transcription", 0, 0)

	if mr.Exists("usage:consumer:transcription:tokens:prompt") {
		t.Error("expected no key created for zero tokens")
	}
}

func TestTrackTokens_AnonymousConsumer_DoesNothing(t *testing.T) {
	tracker, mr := newTracker(t, 0)
	tracker.TrackTokens(context.Background(), "", "transcription", 100, 20)

	if mr.Exists("usage:consumer:transcription:tokens:prompt") {
		t.Error("expected no key created for empty consumer")
	}
}

func TestTrackActive_UpdatesGlobalIndex(t *testing.T) {
	tracker, mr := newTracker(t, 0)
	ctx := context.Background()

	tracker.TrackActive(ctx, "alice")
	score1, _ := mr.ZScore("usage:consumers", "alice")

	mr.FastForward(1 * time.Second)
	// Modify time so the second call has a higher timestamp
	time.Sleep(1 * time.Millisecond) // ensure time.Now().Unix() could differ
	tracker.TrackActive(ctx, "alice")
	score2, _ := mr.ZScore("usage:consumers", "alice")

	// score is a unix timestamp; score2 should be >= score1
	if score2 < score1 {
		t.Errorf("score should not decrease: score1=%v, score2=%v", score1, score2)
	}
}

func TestTrackRequest_WithRetention_SetsTTLOnce(t *testing.T) {
	tracker, mr := newTracker(t, 1*time.Hour)
	ctx := context.Background()

	tracker.TrackRequest(ctx, "alice", "audio")
	ttl1 := mr.TTL("usage:consumer:audio:requests")
	if ttl1 <= 0 {
		t.Fatalf("expected TTL to be set, got %v", ttl1)
	}

	// Second call must NOT reset the TTL (key already has TTL != -1)
	mr.FastForward(30 * time.Second)
	tracker.TrackRequest(ctx, "alice", "audio")
	ttl2 := mr.TTL("usage:consumer:audio:requests")
	if ttl2 >= ttl1 {
		t.Errorf("TTL should have decremented, got ttl2=%v >= ttl1=%v", ttl2, ttl1)
	}
}

func TestTrackRequest_AnonymousConsumer_DoesNothing(t *testing.T) {
	tracker, mr := newTracker(t, 0)
	tracker.TrackRequest(context.Background(), "", "audio")

	// No key should have been created
	keys := mr.Keys()
	if len(keys) != 0 {
		t.Errorf("expected no keys, got %v", keys)
	}
}

func TestTrackProcessingTime_ZeroSeconds_DoesNothing(t *testing.T) {
	tracker, mr := newTracker(t, 0)
	tracker.TrackProcessingTime(context.Background(), "alice", "audio", 0)
	tracker.TrackProcessingTime(context.Background(), "alice", "audio", -1)

	keys := mr.Keys()
	if len(keys) != 0 {
		t.Errorf("expected no keys, got %v", keys)
	}
}

func TestTrackRequest_IncrementsPeriodAggregates(t *testing.T) {
	tracker, mr := newTracker(t, 0)
	ctx := context.Background()
	now := time.Now().UTC()
	year, week := now.ISOWeek()

	tracker.TrackRequest(ctx, "alice", "audio")
	tracker.TrackRequest(ctx, "bob", "audio")

	dailyKey := "usage:agg:audio:requests:daily:" + now.Format("20060102")
	weeklyKey := "usage:agg:audio:requests:weekly:" + weekBucket(year, week)
	monthlyKey := "usage:agg:audio:requests:monthly:" + now.Format("200601")

	for _, key := range []string{dailyKey, weeklyKey, monthlyKey} {
		v, err := mr.Get(key)
		if err != nil {
			t.Fatalf("Get(%s): %v", key, err)
		}
		if v != "2" {
			t.Errorf("%s = %v, want 2 (aggregate across both consumers)", key, v)
		}
	}
}

func TestTrackTokens_IncrementsPeriodAggregates(t *testing.T) {
	tracker, mr := newTracker(t, 0)
	now := time.Now().UTC()

	tracker.TrackTokens(context.Background(), "alice", "llm", 100, 20)

	promptKey := "usage:agg:llm:tokens_prompt:daily:" + now.Format("20060102")
	completionKey := "usage:agg:llm:tokens_completion:daily:" + now.Format("20060102")

	prompt, err := mr.Get(promptKey)
	if err != nil || prompt != "100" {
		t.Errorf("prompt aggregate = %v, %v, want 100", prompt, err)
	}
	completion, err := mr.Get(completionKey)
	if err != nil || completion != "20" {
		t.Errorf("completion aggregate = %v, %v, want 20", completion, err)
	}
}

func TestTrackRequest_PeriodAggregate_TTLSetOnceForDailyAndWeekly_NoneForMonthly(t *testing.T) {
	tracker, mr := newTracker(t, 0)
	ctx := context.Background()
	now := time.Now().UTC()

	tracker.TrackRequest(ctx, "alice", "audio")

	dailyKey := "usage:agg:audio:requests:daily:" + now.Format("20060102")
	monthlyKey := "usage:agg:audio:requests:monthly:" + now.Format("200601")

	if ttl := mr.TTL(dailyKey); ttl <= 0 {
		t.Errorf("expected daily aggregate TTL to be set, got %v", ttl)
	}
	if ttl := mr.TTL(monthlyKey); ttl != 0 {
		t.Errorf("expected no TTL on monthly aggregate, got %v", ttl)
	}

	// Second call must not reset the daily TTL.
	ttl1 := mr.TTL(dailyKey)
	mr.FastForward(30 * time.Second)
	tracker.TrackRequest(ctx, "alice", "audio")
	ttl2 := mr.TTL(dailyKey)
	if ttl2 >= ttl1 {
		t.Errorf("daily aggregate TTL should have decremented, got ttl2=%v >= ttl1=%v", ttl2, ttl1)
	}
}

// weekBucket mirrors the unexported periodBuckets weekly format, used to
// compute the expected key in tests without depending on internal helpers.
func weekBucket(year, week int) string {
	return fmt.Sprintf("%04d-W%02d", year, week)
}

func TestNoopUsageTracker_DoesNothing(t *testing.T) {
	// Must not panic
	usage.NoopUsageTracker.TrackRequest(context.Background(), "alice", "audio")
	usage.NoopUsageTracker.TrackJob(context.Background(), "alice", "audio")
	usage.NoopUsageTracker.TrackProcessingTime(context.Background(), "alice", "audio", 5)
	usage.NoopUsageTracker.TrackTokens(context.Background(), "alice", "audio", 100, 20)
	usage.NoopUsageTracker.TrackActive(context.Background(), "alice")
	usage.NoopUsageTracker.UpdateRetention(time.Hour)
}
