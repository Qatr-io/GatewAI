package llmproxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestProber_OpensThenClosesOnRecovery(t *testing.T) {
	// Fails the first 2 probes (503), then recovers (200).
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&n, 1) <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
		} else {
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	cb := NewCircuitBreaker(2, time.Minute)
	client := &http.Client{Timeout: 2 * time.Second}
	ctx := context.Background()

	probeOne(ctx, cb, client, ProbeTarget{Model: "m", URL: srv.URL})
	probeOne(ctx, cb, client, ProbeTarget{Model: "m", URL: srv.URL})
	if !cb.IsOpen(srv.URL) {
		t.Fatal("circuit should open after 2 failed probes")
	}
	// A successful probe closes it directly (faster than waiting for cooldown).
	probeOne(ctx, cb, client, ProbeTarget{Model: "m", URL: srv.URL})
	if cb.IsOpen(srv.URL) {
		t.Fatal("circuit should close after a successful probe")
	}
}

func TestProber_UnreachableBackendOpens(t *testing.T) {
	cb := NewCircuitBreaker(1, time.Minute)
	client := &http.Client{Timeout: 500 * time.Millisecond}
	// Nothing is listening here → dial error → RecordFailure.
	probeOne(context.Background(), cb, client, ProbeTarget{Model: "m", URL: "http://127.0.0.1:1"})
	if !cb.IsOpen("http://127.0.0.1:1") {
		t.Fatal("an unreachable backend should open after a failed probe")
	}
}
