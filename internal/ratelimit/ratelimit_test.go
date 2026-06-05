package ratelimit_test

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"gatewai/gateway/internal/config"
	"gatewai/gateway/internal/ratelimit"
)

func newLimiter(t *testing.T, limits map[string]map[string]config.RateLimitConfig, consumerHeader, userTypeHeader string) (*ratelimit.Limiter, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return ratelimit.New(rdb, limits, consumerHeader, userTypeHeader), mr
}

func TestCheck_NoConfiguredService_Allowed(t *testing.T) {
	l, _ := newLimiter(t, map[string]map[string]config.RateLimitConfig{}, "X-Consumer", "X-User-Type")
	r := httptest.NewRequest("POST", "/", nil)
	res, err := l.Check(context.Background(), r, "audio")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Allowed {
		t.Fatal("expected allowed when no config for service type")
	}
}

func TestCheck_UnlimitedRate_AlwaysAllowed(t *testing.T) {
	limits := map[string]map[string]config.RateLimitConfig{
		"audio": {
			"unlimited": {Rate: 0},
			"*":         {Rate: 2, Period: "1m"},
		},
	}
	l, _ := newLimiter(t, limits, "X-Consumer", "X-User-Type")

	for i := 0; i < 100; i++ {
		r := httptest.NewRequest("POST", "/", nil)
		r.Header.Set("X-Consumer", "user1")
		r.Header.Set("X-User-Type", "unlimited")
		res, err := l.Check(context.Background(), r, "audio")
		if err != nil {
			t.Fatalf("iteration %d: unexpected error: %v", i, err)
		}
		if !res.Allowed {
			t.Fatalf("iteration %d: expected unlimited user to always be allowed", i)
		}
		if res.Limit != 0 {
			t.Fatalf("iteration %d: expected Limit=0 for unlimited, got %d", i, res.Limit)
		}
	}
}

func TestCheck_RateEnforced_AfterLimit(t *testing.T) {
	limits := map[string]map[string]config.RateLimitConfig{
		"audio": {
			"*": {Rate: 3, Period: "1m"},
		},
	}
	l, _ := newLimiter(t, limits, "X-Consumer", "X-User-Type")

	for i := 1; i <= 3; i++ {
		r := httptest.NewRequest("POST", "/", nil)
		r.Header.Set("X-Consumer", "user1")
		res, err := l.Check(context.Background(), r, "audio")
		if err != nil {
			t.Fatal(err)
		}
		if !res.Allowed {
			t.Fatalf("request %d should be allowed", i)
		}
		if res.Limit != 3 {
			t.Fatalf("request %d: expected Limit=3, got %d", i, res.Limit)
		}
		wantRemaining := 3 - i
		if res.Remaining != wantRemaining {
			t.Fatalf("request %d: expected Remaining=%d, got %d", i, wantRemaining, res.Remaining)
		}
	}

	// 4th request must be rejected.
	r := httptest.NewRequest("POST", "/", nil)
	r.Header.Set("X-Consumer", "user1")
	res, err := l.Check(context.Background(), r, "audio")
	if err != nil {
		t.Fatal(err)
	}
	if res.Allowed {
		t.Fatal("4th request should be rejected")
	}
	if res.Remaining != 0 {
		t.Fatalf("expected Remaining=0 on rejection, got %d", res.Remaining)
	}
	if res.ResetAfter <= 0 {
		t.Fatal("expected positive ResetAfter duration on rejection")
	}
}

func TestCheck_ExactMatchBeforeFallback(t *testing.T) {
	limits := map[string]map[string]config.RateLimitConfig{
		"audio": {
			"premium": {Rate: 10, Period: "1m"},
			"*":       {Rate: 1, Period: "1m"},
		},
	}
	l, _ := newLimiter(t, limits, "X-Consumer", "X-User-Type")

	// premium user gets rate 10, so 2 requests must both be allowed.
	for i := 0; i < 2; i++ {
		r := httptest.NewRequest("POST", "/", nil)
		r.Header.Set("X-Consumer", "puser")
		r.Header.Set("X-User-Type", "premium")
		res, err := l.Check(context.Background(), r, "audio")
		if err != nil {
			t.Fatal(err)
		}
		if !res.Allowed {
			t.Fatalf("premium request %d should be allowed", i+1)
		}
	}

	// default user hits limit after 1 request.
	r1 := httptest.NewRequest("POST", "/", nil)
	r1.Header.Set("X-Consumer", "duser")
	res1, err := l.Check(context.Background(), r1, "audio")
	if err != nil {
		t.Fatal(err)
	}
	if !res1.Allowed {
		t.Fatal("1st default request should be allowed")
	}

	r2 := httptest.NewRequest("POST", "/", nil)
	r2.Header.Set("X-Consumer", "duser")
	res2, err2 := l.Check(context.Background(), r2, "audio")
	if err2 != nil {
		t.Fatal(err2)
	}
	if res2.Allowed {
		t.Fatal("2nd default request should be rejected")
	}
}

func TestCheck_NoUserTypeHeader_UsesFallback(t *testing.T) {
	limits := map[string]map[string]config.RateLimitConfig{
		"audio": {
			"*": {Rate: 1, Period: "1m"},
		},
	}
	l, _ := newLimiter(t, limits, "X-Consumer", "X-User-Type")

	r := httptest.NewRequest("POST", "/", nil)
	r.Header.Set("X-Consumer", "u1")
	res, err := l.Check(context.Background(), r, "audio")
	if err != nil || !res.Allowed {
		t.Fatalf("first request should pass: allowed=%v err=%v", res.Allowed, err)
	}

	r2 := httptest.NewRequest("POST", "/", nil)
	r2.Header.Set("X-Consumer", "u1")
	res2, _ := l.Check(context.Background(), r2, "audio")
	if res2.Allowed {
		t.Fatal("second request should be rejected by fallback limit")
	}
}

func TestCheck_RateLimitHeaders_ResetAfterPositive(t *testing.T) {
	limits := map[string]map[string]config.RateLimitConfig{
		"audio": {
			"*": {Rate: 5, Period: "1m"},
		},
	}
	l, _ := newLimiter(t, limits, "X-Consumer", "X-User-Type")

	r := httptest.NewRequest("POST", "/", nil)
	r.Header.Set("X-Consumer", "u1")
	res, err := l.Check(context.Background(), r, "audio")
	if err != nil {
		t.Fatal(err)
	}
	if res.ResetAfter <= 0 {
		t.Fatalf("expected positive ResetAfter, got %v", res.ResetAfter)
	}
	if res.Limit != 5 {
		t.Fatalf("expected Limit=5, got %d", res.Limit)
	}
	if res.Remaining != 4 {
		t.Fatalf("expected Remaining=4, got %d", res.Remaining)
	}
}
