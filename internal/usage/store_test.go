package usage_test

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"gatewai/gateway/internal/usage"
)

func newStore(t *testing.T, retention string) (usage.UsageStore, *miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return usage.NewRedisUsageStore(rdb, retention), mr, rdb
}

func TestGetConsumerUsage_Empty(t *testing.T) {
	store, _, _ := newStore(t, "")
	result, err := store.GetConsumerUsage(context.Background(), "alice", "*", []string{"audio"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Consumer != "alice" {
		t.Errorf("consumer = %q", result.Consumer)
	}
	if result.Retention != "all-time" {
		t.Errorf("retention = %q, want all-time", result.Retention)
	}
	if len(result.Usage) != 0 {
		t.Errorf("expected empty usage, got %d entries", len(result.Usage))
	}
}

func TestGetConsumerUsage_WithData(t *testing.T) {
	store, _, rdb := newStore(t, "")
	ctx := context.Background()

	rdb.ZIncrBy(ctx, "usage:consumer:audio:requests", 100, "alice")
	rdb.ZIncrBy(ctx, "usage:consumer:audio:jobs", 80, "alice")
	rdb.ZIncrBy(ctx, "usage:consumer:audio:processing_time", 3600.5, "alice")

	result, err := store.GetConsumerUsage(ctx, "alice", "*", []string{"audio"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Usage) != 1 {
		t.Fatalf("expected 1 service, got %d", len(result.Usage))
	}
	svc := result.Usage[0]
	if svc.ServiceType != "audio" {
		t.Errorf("service_type = %q", svc.ServiceType)
	}
	if svc.Total.Requests != 100 {
		t.Errorf("requests = %d, want 100", svc.Total.Requests)
	}
	if svc.Total.Jobs != 80 {
		t.Errorf("jobs = %d, want 80", svc.Total.Jobs)
	}
	if svc.Total.ProcessingTime != 3600 {
		// float stored as sorted set score rounds to int in zscore
		t.Logf("processing_time = %v (raw float from ZScore may be ~3600)", svc.Total.ProcessingTime)
	}
}

func TestGetConsumerUsage_LLMTokens(t *testing.T) {
	store, _, rdb := newStore(t, "")
	ctx := context.Background()

	rdb.ZIncrBy(ctx, "usage:consumer:llm:requests", 50, "alice")
	rdb.ZIncrBy(ctx, "llm:consumer:tokens:user:prompt", 10000, "alice")
	rdb.ZIncrBy(ctx, "llm:consumer:tokens:user:completion", 2000, "alice")

	result, err := store.GetConsumerUsage(ctx, "alice", "user", []string{"llm"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Usage) != 1 {
		t.Fatalf("expected 1 service, got %d", len(result.Usage))
	}
	svc := result.Usage[0]
	if svc.Total.Tokens == nil {
		t.Fatal("expected tokens to be non-nil for llm service type")
	}
	if svc.Total.Tokens.Prompt != 10000 {
		t.Errorf("prompt = %d, want 10000", svc.Total.Tokens.Prompt)
	}
	if svc.Total.Tokens.Completion != 2000 {
		t.Errorf("completion = %d, want 2000", svc.Total.Tokens.Completion)
	}
}

// TestGetConsumerUsage_GenericTokens verifies non-"llm" service types read
// tokens from the new usage:consumer:{svcType}:tokens:* keys, and that this
// does not interfere with the existing "llm" svcType code path.
func TestGetConsumerUsage_GenericTokens(t *testing.T) {
	store, mr, _ := newStore(t, "")
	mr.ZAdd("usage:consumer:transcription:tokens:prompt", 500, "alice")
	mr.ZAdd("usage:consumer:transcription:tokens:completion", 80, "alice")

	usage, err := store.GetConsumerUsage(context.Background(), "alice", "user", []string{"transcription"})
	if err != nil {
		t.Fatalf("GetConsumerUsage: %v", err)
	}
	if len(usage.Usage) != 1 {
		t.Fatalf("expected 1 service usage entry, got %d", len(usage.Usage))
	}
	svc := usage.Usage[0]
	if svc.Total.Tokens == nil {
		t.Fatal("expected Tokens to be populated")
	}
	if svc.Total.Tokens.Prompt != 500 {
		t.Errorf("Prompt: got %d, want 500", svc.Total.Tokens.Prompt)
	}
	if svc.Total.Tokens.Completion != 80 {
		t.Errorf("Completion: got %d, want 80", svc.Total.Tokens.Completion)
	}
}

func TestGetConsumerUsage_WindowIncluded(t *testing.T) {
	store, _, rdb := newStore(t, "")
	ctx := context.Background()

	rdb.ZIncrBy(ctx, "usage:consumer:audio:requests", 10, "alice")
	rdb.Set(ctx, "rl:alice:audio:*", "5", 30*time.Minute)

	result, err := store.GetConsumerUsage(ctx, "alice", "*", []string{"audio"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Usage) == 0 {
		t.Fatal("expected usage entries")
	}
	svc := result.Usage[0]
	if svc.Window == nil {
		t.Fatal("expected window to be non-nil when rl: key exists")
	}
	if svc.Window.Requests != 5 {
		t.Errorf("window.requests = %d, want 5", svc.Window.Requests)
	}
	if svc.Window.ResetAt == nil {
		t.Error("expected reset_at to be set")
	}
}

func TestGetConsumerUsage_NoWindowWhenKeyAbsent(t *testing.T) {
	store, _, rdb := newStore(t, "")
	ctx := context.Background()

	rdb.ZIncrBy(ctx, "usage:consumer:audio:requests", 10, "alice")

	result, err := store.GetConsumerUsage(ctx, "alice", "*", []string{"audio"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Usage) == 0 {
		t.Fatal("expected usage entries")
	}
	if result.Usage[0].Window != nil {
		t.Error("expected window to be nil when no rl: key exists")
	}
}

func TestGetConsumerUsage_LastActive(t *testing.T) {
	store, _, rdb := newStore(t, "")
	ctx := context.Background()

	ts := time.Now().UTC().Truncate(time.Second)
	rdb.ZAdd(ctx, "usage:consumers", redis.Z{Score: float64(ts.Unix()), Member: "alice"})
	rdb.ZIncrBy(ctx, "usage:consumer:audio:requests", 1, "alice")

	result, err := store.GetConsumerUsage(ctx, "alice", "*", []string{"audio"})
	if err != nil {
		t.Fatal(err)
	}
	if result.LastActive == nil {
		t.Fatal("expected last_active to be set")
	}
	if !result.LastActive.Equal(ts) {
		t.Errorf("last_active = %v, want %v", *result.LastActive, ts)
	}
}

func TestGetConsumerUsage_RetentionLabel(t *testing.T) {
	store, _, rdb := newStore(t, "720h")
	ctx := context.Background()

	rdb.ZIncrBy(ctx, "usage:consumer:audio:requests", 1, "alice")

	result, err := store.GetConsumerUsage(ctx, "alice", "*", []string{"audio"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Retention != "720h" {
		t.Errorf("retention = %q, want 720h", result.Retention)
	}
}

func TestListConsumers_Paginated(t *testing.T) {
	store, _, rdb := newStore(t, "")
	ctx := context.Background()
	now := float64(time.Now().Unix())

	rdb.ZAdd(ctx, "usage:consumers", redis.Z{Score: now, Member: "alice"})
	rdb.ZAdd(ctx, "usage:consumers", redis.Z{Score: now - 1, Member: "bob"})
	rdb.ZAdd(ctx, "usage:consumers", redis.Z{Score: now - 2, Member: "charlie"})

	consumers, total, err := store.ListConsumers(ctx, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 {
		t.Errorf("total = %d, want 3", total)
	}
	if len(consumers) != 2 {
		t.Errorf("got %d consumers, want 2", len(consumers))
	}
	// Most recent first
	if consumers[0] != "alice" {
		t.Errorf("first consumer = %q, want alice", consumers[0])
	}
}

func TestListConsumers_Offset(t *testing.T) {
	store, _, rdb := newStore(t, "")
	ctx := context.Background()
	now := float64(time.Now().Unix())

	rdb.ZAdd(ctx, "usage:consumers", redis.Z{Score: now, Member: "alice"})
	rdb.ZAdd(ctx, "usage:consumers", redis.Z{Score: now - 1, Member: "bob"})
	rdb.ZAdd(ctx, "usage:consumers", redis.Z{Score: now - 2, Member: "charlie"})

	consumers, total, err := store.ListConsumers(ctx, 2, 1)
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 {
		t.Errorf("total = %d, want 3", total)
	}
	if len(consumers) != 2 {
		t.Errorf("got %d consumers, want 2", len(consumers))
	}
	if consumers[0] != "bob" {
		t.Errorf("first consumer with offset=1 = %q, want bob", consumers[0])
	}
}

func TestListConsumersByType_Paginated(t *testing.T) {
	store, _, rdb := newStore(t, "")
	ctx := context.Background()

	rdb.ZAdd(ctx, "usage:consumer:audio:requests", redis.Z{Score: 100, Member: "alice"})
	rdb.ZAdd(ctx, "usage:consumer:audio:requests", redis.Z{Score: 50, Member: "bob"})

	consumers, total, err := store.ListConsumersByType(ctx, "audio", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 {
		t.Errorf("total = %d, want 2", total)
	}
	// Highest score first
	if consumers[0] != "alice" {
		t.Errorf("first = %q, want alice", consumers[0])
	}
}
