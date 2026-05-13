// Package health tracks per-backend error rates using an EWMA and exposes
// effective routing weights for health-aware load balancing.
package health

import (
	"sync"

	"kevent/gateway/internal/service"
)

const alpha = 0.1

// BackendHealth tracks an exponentially-weighted moving average (EWMA) of the
// error rate per backend URL. A nil *BackendHealth is valid and acts as a no-op.
type BackendHealth struct {
	mu    sync.RWMutex
	rates map[string]float64 // EWMA error rate in [0, 1] per URL
}

func New() *BackendHealth {
	return &BackendHealth{rates: make(map[string]float64)}
}

// RecordResult updates the EWMA for url.
// success=false means a network error or 5xx response.
func (h *BackendHealth) RecordResult(url string, success bool) {
	if h == nil {
		return
	}
	sample := 0.0
	if !success {
		sample = 1.0
	}
	h.mu.Lock()
	old, exists := h.rates[url]
	if !exists {
		h.rates[url] = sample
	} else {
		h.rates[url] = alpha*sample + (1-alpha)*old
	}
	h.mu.Unlock()
}

// ErrorRate returns the current EWMA error rate for url (0.0..1.0).
// Returns 0 for unknown URLs.
func (h *BackendHealth) ErrorRate(url string) float64 {
	if h == nil {
		return 0
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.rates[url]
}

// EffectiveWeight returns the routing weight to use given the current error rate:
//   - errorRate > 0.5 → 0 (excluded from primary selection)
//   - errorRate > 0.2 → configuredWeight/2 (penalised, min 1 when configured > 0)
//   - otherwise → configuredWeight (unchanged)
func (h *BackendHealth) EffectiveWeight(url string, configuredWeight int) int {
	rate := h.ErrorRate(url)
	switch {
	case rate > 0.5:
		return 0
	case rate > 0.2:
		w := configuredWeight / 2
		if w < 1 && configuredWeight > 0 {
			w = 1
		}
		return w
	default:
		return configuredWeight
	}
}

// AdjustedBackends returns a copy of backends with weights adjusted by the
// current health state. Backends with configuredWeight=0 are never penalised
// further (they remain last-resort fallbacks). Returns backends unchanged when
// h is nil.
func (h *BackendHealth) AdjustedBackends(backends []service.Backend) []service.Backend {
	if h == nil || len(backends) == 0 {
		return backends
	}
	out := make([]service.Backend, len(backends))
	copy(out, backends)
	for i, b := range out {
		if b.Weight > 0 {
			out[i].Weight = h.EffectiveWeight(b.URL, b.Weight)
		}
	}
	return out
}
