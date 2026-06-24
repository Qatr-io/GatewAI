package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"gatewai/gateway/internal/health"
)

// Health handles GET /health — lightweight probe with no backend checks.
// Used directly in unit tests; production routes use NewHealthHandler.
func Health(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status": "ok",
		"time":   time.Now().UTC(),
	})
}

// NewHealthHandler returns an http.HandlerFunc implementing GET /health with
// optional query params:
//   - ?verbose=true     — include per-backend status and last check timestamp
//   - ?mode=strict      — return 500 if any backend is down (requires verbose=true or model=)
//   - ?model=<name>     — filter to a single model
//
// snapshotFn is called to retrieve the latest health snapshot from the cache.
// Pass checker.Snapshot for production; inject a test function in tests.
// Without any params the response is the lightweight {"status":"ok","time":...} 200.
func NewHealthHandler(snapshotFn func(context.Context) (*health.Snapshot, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		verbose := q.Get("verbose") == "true"
		strict := q.Get("mode") == "strict"
		modelFilter := q.Get("model")

		if !verbose && modelFilter == "" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "ok",
				"time":   time.Now().UTC(),
			})
			return
		}

		snap, err := snapshotFn(r.Context())
		if err != nil {
			// Redis unavailable: degrade gracefully.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "unknown",
				"error":  err.Error(),
			})
			return
		}

		// No snapshot yet (first startup, before the first probe cycle).
		if snap == nil {
			w.Header().Set("Content-Type", "application/json")
			if strict {
				w.WriteHeader(http.StatusInternalServerError)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "unknown",
			})
			return
		}

		// Filter backends by model if requested.
		backends := snap.Backends
		if modelFilter != "" {
			if s, ok := snap.Backends[modelFilter]; ok {
				backends = map[string]string{modelFilter: s}
			} else {
				backends = map[string]string{}
			}
		}

		// Derive aggregate: only "up" when at least one backend was probed and
		// none returned "down". No probed backends → "unknown".
		aggStatus := aggregateStatus(backends)

		w.Header().Set("Content-Type", "application/json")
		if strict && aggStatus != "up" {
			w.WriteHeader(http.StatusInternalServerError)
		}

		body := map[string]any{
			"status":     aggStatus,
			"checked_at": snap.CheckedAt,
		}
		if verbose {
			body["backends"] = backends
		}
		_ = json.NewEncoder(w).Encode(body)
	}
}

// aggregateStatus derives the overall health from a backends map.
//   - "up"      — at least one backend probed, all are up
//   - "partial" — some backends up, some down (≥2 backends)
//   - "down"    — all probed backends are down
//   - "unknown" — nothing was probed (empty map)
func aggregateStatus(backends map[string]string) string {
	if len(backends) == 0 {
		return "unknown"
	}
	var up, down int
	for _, s := range backends {
		switch s {
		case "up":
			up++
		case "down":
			down++
		}
	}
	switch {
	case down > 0 && up > 0:
		return "partial"
	case down > 0:
		return "down"
	default:
		return "up"
	}
}
