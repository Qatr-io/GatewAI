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
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "up",
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

		anyDown := false
		for _, s := range backends {
			if s == "down" {
				anyDown = true
				break
			}
		}

		aggStatus := "up"
		if anyDown {
			aggStatus = "down"
		}

		w.Header().Set("Content-Type", "application/json")
		if strict && anyDown {
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
