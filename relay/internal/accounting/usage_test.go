package accounting_test

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"gatewai/relay/internal/accounting"
)

func newTestTracker(t *testing.T, retention time.Duration) (*accounting.Tracker, *miniredis.Miniredis) {
	t.Helper()
	rdb, mr := newTestRedis(t)
	return accounting.NewTracker(rdb, retention), mr
}

func TestTrackProcessingTime_AccumulatesSeconds(t *testing.T) {
	tracker, mr := newTestTracker(t, 0)
	tracker.TrackProcessingTime(context.Background(), "alice", "transcription", 30.5)
	tracker.TrackProcessingTime(context.Background(), "alice", "transcription", 10.0)

	score, err := mr.ZScore("usage:consumer:transcription:processing_time", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if score != 40.5 {
		t.Errorf("got %v, want 40.5", score)
	}
}

func TestTrackProcessingTime_SkipsZeroOrEmptyConsumer(t *testing.T) {
	tracker, mr := newTestTracker(t, 0)
	tracker.TrackProcessingTime(context.Background(), "", "transcription", 30.5)
	tracker.TrackProcessingTime(context.Background(), "alice", "transcription", 0)

	if mr.Exists("usage:consumer:transcription:processing_time") {
		t.Error("expected no key created")
	}
}

func TestTrackTokens_SplitsPromptAndCompletion(t *testing.T) {
	tracker, mr := newTestTracker(t, 0)
	tracker.TrackTokens(context.Background(), "alice", "llm", 100, 20)

	promptScore, _ := mr.ZScore("usage:consumer:llm:tokens:prompt", "alice")
	completionScore, _ := mr.ZScore("usage:consumer:llm:tokens:completion", "alice")
	if promptScore != 100 || completionScore != 20 {
		t.Errorf("got prompt=%v completion=%v, want 100/20", promptScore, completionScore)
	}
}

func TestTrackActive_SetsLastActiveTimestamp(t *testing.T) {
	tracker, mr := newTestTracker(t, 0)
	tracker.TrackActive(context.Background(), "alice")

	if !mr.Exists("usage:consumers") {
		t.Fatal("expected usage:consumers key to exist")
	}
	score, err := mr.ZScore("usage:consumers", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if score <= 0 {
		t.Errorf("expected positive timestamp score, got %v", score)
	}
}

func TestTrackUserType_SetsHashField(t *testing.T) {
	tracker, mr := newTestTracker(t, 0)
	tracker.TrackUserType(context.Background(), "alice", "transcription", "user")

	val := mr.HGet("usage:consumer:transcription:usertype", "alice")
	if val != "user" {
		t.Errorf("got %q, want \"user\"", val)
	}
}

func TestTrackProcessingTime_SetsTTLOnlyOnFirstWrite(t *testing.T) {
	tracker, mr := newTestTracker(t, time.Hour)
	tracker.TrackProcessingTime(context.Background(), "alice", "transcription", 5)
	firstTTL := mr.TTL("usage:consumer:transcription:processing_time")
	if firstTTL <= 0 {
		t.Fatalf("expected positive TTL after first write, got %v", firstTTL)
	}

	mr.SetTTL("usage:consumer:transcription:processing_time", 5*time.Minute) // simulate elapsed time
	tracker.TrackProcessingTime(context.Background(), "alice", "transcription", 5)
	if got := mr.TTL("usage:consumer:transcription:processing_time"); got != 5*time.Minute {
		t.Errorf("expected TTL untouched at 5m after second write, got %v", got)
	}
}
