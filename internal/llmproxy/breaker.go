package llmproxy

import (
	"sync"
	"time"

	"gatewai/gateway/internal/metrics"
)

// CircuitBreaker tracks per-backend health for the LLM proxy. After a run of
// consecutive failures a backend's circuit opens and requests skip it until a
// cooldown elapses, at which point a half-open probe is let through: a success
// closes the circuit, a failure re-opens it for another cooldown.
//
// State is in-memory per gateway replica (each replica learns independently,
// which with a small replica count is cheap and avoids a hot-path Redis call).
// Keyed by backend URL; the model label is passed through for metrics only.
// The zero value is not usable — call NewCircuitBreaker.
type CircuitBreaker struct {
	mu        sync.Mutex
	states    map[string]*backendState
	threshold int
	cooldown  time.Duration
}

type backendState struct {
	failures  int
	open      bool
	openUntil time.Time // when a half-open probe becomes allowed
}

// NewCircuitBreaker returns a breaker that opens a backend after `threshold`
// consecutive failures and keeps it open for `cooldown`.
func NewCircuitBreaker(threshold int, cooldown time.Duration) *CircuitBreaker {
	if threshold <= 0 {
		threshold = 5
	}
	if cooldown <= 0 {
		cooldown = 30 * time.Second
	}
	return &CircuitBreaker{
		states:    make(map[string]*backendState),
		threshold: threshold,
		cooldown:  cooldown,
	}
}

// Allow reports whether a request may be sent to the backend. A closed circuit
// always allows; an open circuit allows only once its cooldown has elapsed (a
// half-open probe).
func (cb *CircuitBreaker) Allow(url string) bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	st := cb.states[url]
	if st == nil || !st.open {
		return true
	}
	return !time.Now().Before(st.openUntil) // open, but cooldown elapsed → probe
}

// RecordSuccess marks a successful call, closing the circuit and clearing the
// failure count.
func (cb *CircuitBreaker) RecordSuccess(model, url string) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	st := cb.states[url]
	if st == nil {
		return
	}
	if st.open {
		metrics.BackendCircuitOpen.WithLabelValues(model, url).Set(0)
	}
	st.failures = 0
	st.open = false
}

// RecordFailure marks a failed call. Once consecutive failures reach the
// threshold the circuit opens; a failure while open (a failed probe) extends
// the cooldown.
func (cb *CircuitBreaker) RecordFailure(model, url string) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	st := cb.states[url]
	if st == nil {
		st = &backendState{}
		cb.states[url] = st
	}
	st.failures++
	switch {
	case st.open:
		// Failed half-open probe → keep open, restart the cooldown.
		st.openUntil = time.Now().Add(cb.cooldown)
	case st.failures >= cb.threshold:
		st.open = true
		st.openUntil = time.Now().Add(cb.cooldown)
		metrics.BackendCircuitOpen.WithLabelValues(model, url).Set(1)
		metrics.BackendCircuitOpensTotal.WithLabelValues(model, url).Inc()
	}
}

// IsOpen reports whether the backend's circuit is currently open (skipping
// requests, cooldown not yet elapsed). Used to surface degraded backends.
func (cb *CircuitBreaker) IsOpen(url string) bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	st := cb.states[url]
	return st != nil && st.open && time.Now().Before(st.openUntil)
}
