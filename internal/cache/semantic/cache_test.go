package semantic_test

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"kevent/gateway/internal/cache"
	"kevent/gateway/internal/cache/semantic"
)

// ── helpers ──────────────────────────────────────────────────────────────────

func newTestCache(t *testing.T, threshold float64) (*semantic.Cache, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	// Static embedder: returns a fixed 4-dim unit vector for any text.
	embed := func(_ context.Context, _ string) ([]float32, error) {
		return []float32{1, 0, 0, 0}, nil
	}
	return semantic.New(rdb, embed, threshold, 4), mr
}

var fakeEntry = &cache.Entry{
	Body:        []byte(`{"id":"r1","object":"chat.completion"}`),
	ContentType: "application/json",
	StatusCode:  200,
}

const msgBody = `{"messages":[{"role":"user","content":"Hello world"}]}`

// ── tests ────────────────────────────────────────────────────────────────────

func TestNilCache_GetSetNoop(t *testing.T) {
	var c *semantic.Cache
	entry, hit, err := c.Get(context.Background(), []byte(msgBody))
	if err != nil || hit || entry != nil {
		t.Errorf("nil cache Get should no-op: entry=%v hit=%v err=%v", entry, hit, err)
	}
	if err := c.Set(context.Background(), []byte(msgBody), fakeEntry, time.Hour); err != nil {
		t.Errorf("nil cache Set should no-op: %v", err)
	}
}

func TestGet_NoIndexYet_MissNotError(t *testing.T) {
	c, _ := newTestCache(t, 0.95)
	// Index does not exist yet in miniredis — should be created by Get.
	// miniredis doesn't support RediSearch, so ensureIndex will fail silently.
	entry, hit, err := c.Get(context.Background(), []byte(msgBody))
	// err is acceptable (Redis Stack unavailable); hit must be false.
	if hit {
		t.Error("expected cache miss, got hit")
	}
	if entry != nil {
		t.Error("expected nil entry on miss")
	}
	_ = err // tolerate error when RediSearch is unavailable
}

func TestExtractText_EmptyBody_NoViolation(t *testing.T) {
	c, _ := newTestCache(t, 0.95)
	// Empty body — extractText returns "" and Get returns miss without error.
	entry, hit, err := c.Get(context.Background(), []byte{})
	if hit || entry != nil || err != nil {
		t.Errorf("empty body should be clean miss: hit=%v entry=%v err=%v", hit, entry, err)
	}
}

func TestGet_NonMessagePayload_CleanMiss(t *testing.T) {
	c, _ := newTestCache(t, 0.95)
	body := []byte(`{"prompt":"hello","max_tokens":100}`)
	entry, hit, err := c.Get(context.Background(), body)
	if hit || entry != nil || err != nil {
		t.Errorf("non-message payload should be clean miss: hit=%v entry=%v err=%v", hit, entry, err)
	}
}
