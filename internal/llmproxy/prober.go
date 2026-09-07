package llmproxy

import (
	"context"
	"io"
	"net/http"
	"strings"
	"time"
)

// ProbeTarget is one backend to actively health-check.
type ProbeTarget struct {
	Model      string
	URL        string // backend base URL
	HealthPath string // path appended to URL (default "/health")
}

// StartProber runs a per-replica loop that probes each target's health endpoint
// every interval and feeds the breaker directly (RecordSuccess/RecordFailure),
// so a dead backend opens and a recovered one closes without waiting for live
// request traffic. targets() is called each cycle so it reflects registry
// hot-reloads. The loop stops when ctx is cancelled.
//
// Unlike the shared /health checker (one replica probes per cycle via a Redis
// lock), this is intentionally per-replica: the circuit breaker state is
// per-replica, so each replica must probe to keep its own view current.
func StartProber(ctx context.Context, cb *CircuitBreaker, client *http.Client, interval time.Duration, targets func() []ProbeTarget) {
	if cb == nil || interval <= 0 || targets == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				for _, t := range targets() {
					probeOne(ctx, cb, client, t)
				}
			}
		}
	}()
}

func probeOne(ctx context.Context, cb *CircuitBreaker, client *http.Client, t ProbeTarget) {
	path := t.HealthPath
	if path == "" {
		path = "/health"
	}
	url := strings.TrimSuffix(t.URL, "/") + "/" + strings.TrimPrefix(path, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return
	}
	resp, err := client.Do(req)
	if err != nil {
		cb.RecordFailure(t.Model, t.URL)
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode >= 500 {
		cb.RecordFailure(t.Model, t.URL)
		return
	}
	cb.RecordSuccess(t.Model, t.URL)
}
