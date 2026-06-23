package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"gatewai/gateway/internal/config"
	"gatewai/gateway/internal/service"
)

const (
	healthDefaultTimeout = 5 * time.Second
	healthDefaultPath    = "/health"
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

type healthHandler struct {
	reg            *service.Registry
	defaultTimeout time.Duration
	client         *http.Client
}

// NewHealthHandler returns an http.HandlerFunc implementing GET /health with
// optional query params:
//   - ?verbose=true        — probe all backends and return per-model status
//   - ?mode=strict         — return 500 if any backend is down (requires verbose=true or model=)
//   - ?model=<name>        — probe only the named model
//
// Without any params the response is the lightweight {"status":"ok","time":...} 200.
func NewHealthHandler(reg *service.Registry, cfg config.HealthConfig) http.HandlerFunc {
	timeout := cfg.TimeoutDuration()
	if timeout <= 0 {
		timeout = healthDefaultTimeout
	}
	h := &healthHandler{
		reg:            reg,
		defaultTimeout: timeout,
		client:         &http.Client{},
	}
	return h.ServeHTTP
}

func (h *healthHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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

	defs := h.reg.All()
	if modelFilter != "" {
		var filtered []*service.Def
		for _, d := range defs {
			if d.Model == modelFilter {
				filtered = append(filtered, d)
				break
			}
		}
		defs = filtered
	}

	backendStatus := make(map[string]string, len(defs))
	anyDown := false
	for _, d := range defs {
		if d.InferenceURL == "" || d.HealthCheck.Disabled {
			continue
		}
		key := d.Model
		if key == "" {
			key = d.Type
		}
		s := h.probe(r.Context(), d)
		backendStatus[key] = s
		if s == "down" {
			anyDown = true
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

	body := map[string]any{"status": aggStatus}
	if verbose {
		body["backends"] = backendStatus
	}
	_ = json.NewEncoder(w).Encode(body)
}

// probe makes a HEAD request to the backend's health endpoint and returns "up" or "down".
// Status codes 2xx–4xx are treated as "up" (the backend is responding).
func (h *healthHandler) probe(ctx context.Context, d *service.Def) string {
	timeout := h.defaultTimeout
	if t := d.HealthCheck.TimeoutDuration(); t > 0 {
		timeout = t
	}

	healthPath := d.HealthCheck.Path
	if healthPath == "" {
		healthPath = healthDefaultPath
	}
	probeURL := strings.TrimSuffix(d.InferenceURL, "/") + "/" + strings.TrimPrefix(healthPath, "/")

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, probeURL, nil)
	if err != nil {
		return "down"
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return "down"
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 500 {
		return "up"
	}
	return "down"
}
