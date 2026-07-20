package accounting_test

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"gatewai/relay/internal/accounting"
	"gatewai/relay/internal/config"
)

func newTestRedis(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb, mr
}

func TestAddTokens_IncrementsAndSetsTTL(t *testing.T) {
	rdb, mr := newTestRedis(t)
	limits := map[string]map[string]config.RateLimitConfig{
		"transcription": {"user": {TokenRate: 100000, TokenPeriod: "24h"}},
	}
	l := accounting.NewLimiter(limits)

	if err := l.AddTokens(context.Background(), rdb, "alice", "user", "transcription", 42); err != nil {
		t.Fatalf("AddTokens: %v", err)
	}

	got, err := mr.Get("trl:alice:transcription:user")
	if err != nil {
		t.Fatalf("key not found: %v", err)
	}
	if got != "42" {
		t.Errorf("got %q, want \"42\"", got)
	}
	ttl := mr.TTL("trl:alice:transcription:user")
	if ttl <= 0 {
		t.Errorf("expected positive TTL, got %v", ttl)
	}
}

func TestAddTokens_ZeroTotal_NoOp(t *testing.T) {
	rdb, mr := newTestRedis(t)
	limits := map[string]map[string]config.RateLimitConfig{
		"transcription": {"user": {TokenRate: 100000, TokenPeriod: "24h"}},
	}
	l := accounting.NewLimiter(limits)

	if err := l.AddTokens(context.Background(), rdb, "alice", "user", "transcription", 0); err != nil {
		t.Fatalf("AddTokens: %v", err)
	}
	if mr.Exists("trl:alice:transcription:user") {
		t.Error("expected no key created for zero total")
	}
}

func TestAddTokens_NoLimitConfigured_NoOp(t *testing.T) {
	rdb, mr := newTestRedis(t)
	l := accounting.NewLimiter(nil)

	if err := l.AddTokens(context.Background(), rdb, "alice", "user", "transcription", 42); err != nil {
		t.Fatalf("AddTokens: %v", err)
	}
	if mr.Exists("trl:alice:transcription:user") {
		t.Error("expected no key created when no limit is configured")
	}
}

func TestAddTokens_UserTypeFallback(t *testing.T) {
	rdb, mr := newTestRedis(t)
	limits := map[string]map[string]config.RateLimitConfig{
		"transcription": {"*": {TokenRate: 100000, TokenPeriod: "24h"}},
	}
	l := accounting.NewLimiter(limits)

	if err := l.AddTokens(context.Background(), rdb, "alice", "unknown-tier", "transcription", 10); err != nil {
		t.Fatalf("AddTokens: %v", err)
	}
	if !mr.Exists("trl:alice:transcription:unknown-tier") {
		t.Error("expected fallback \"*\" config to apply")
	}
}

func TestAddProcessingTime_IncrementsCeiledSeconds(t *testing.T) {
	rdb, mr := newTestRedis(t)
	limits := map[string]map[string]config.RateLimitConfig{
		"transcription": {"user": {ProcessingTime: 3600, ProcessingPeriod: "24h"}},
	}
	l := accounting.NewLimiter(limits)

	if err := l.AddProcessingTime(context.Background(), rdb, "alice", "user", "transcription", 9.2); err != nil {
		t.Fatalf("AddProcessingTime: %v", err)
	}
	got, err := mr.Get("ptrl:alice:transcription:user")
	if err != nil {
		t.Fatalf("key not found: %v", err)
	}
	if got != "10" { // math.Ceil(9.2) == 10
		t.Errorf("got %q, want \"10\"", got)
	}
}

func TestAddProcessingTime_ZeroSeconds_NoOp(t *testing.T) {
	rdb, mr := newTestRedis(t)
	limits := map[string]map[string]config.RateLimitConfig{
		"transcription": {"user": {ProcessingTime: 3600, ProcessingPeriod: "24h"}},
	}
	l := accounting.NewLimiter(limits)

	if err := l.AddProcessingTime(context.Background(), rdb, "alice", "user", "transcription", 0); err != nil {
		t.Fatalf("AddProcessingTime: %v", err)
	}
	if mr.Exists("ptrl:alice:transcription:user") {
		t.Error("expected no key created for zero seconds")
	}
}

func TestAddTokens_SecondCallIncrementsWithoutResettingTTL(t *testing.T) {
	rdb, mr := newTestRedis(t)
	limits := map[string]map[string]config.RateLimitConfig{
		"transcription": {"user": {TokenRate: 100000, TokenPeriod: "24h"}},
	}
	l := accounting.NewLimiter(limits)

	if err := l.AddTokens(context.Background(), rdb, "alice", "user", "transcription", 10); err != nil {
		t.Fatalf("AddTokens #1: %v", err)
	}
	mr.SetTTL("trl:alice:transcription:user", time.Hour) // simulate elapsed time
	if err := l.AddTokens(context.Background(), rdb, "alice", "user", "transcription", 5); err != nil {
		t.Fatalf("AddTokens #2: %v", err)
	}
	got, _ := mr.Get("trl:alice:transcription:user")
	if got != "15" {
		t.Errorf("got %q, want \"15\"", got)
	}
	if ttl := mr.TTL("trl:alice:transcription:user"); ttl != time.Hour {
		t.Errorf("expected TTL to remain untouched at 1h, got %v", ttl)
	}
}
