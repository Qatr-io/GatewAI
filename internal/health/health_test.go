package health_test

import (
	"testing"

	"kevent/gateway/internal/health"
	"kevent/gateway/internal/service"
)

func TestRecordResult_NilSafe(t *testing.T) {
	var h *health.BackendHealth
	h.RecordResult("http://backend", false) // must not panic
}

func TestErrorRate_UnknownURL_ReturnsZero(t *testing.T) {
	h := health.New()
	if r := h.ErrorRate("http://unknown"); r != 0 {
		t.Errorf("expected 0 for unknown URL, got %v", r)
	}
}

func TestErrorRate_FirstSample(t *testing.T) {
	h := health.New()
	h.RecordResult("http://b", false)
	if r := h.ErrorRate("http://b"); r != 1.0 {
		t.Errorf("first error sample should set rate to 1.0, got %v", r)
	}

	h2 := health.New()
	h2.RecordResult("http://b", true)
	if r := h2.ErrorRate("http://b"); r != 0.0 {
		t.Errorf("first success sample should set rate to 0.0, got %v", r)
	}
}

func TestErrorRate_EWMA_Decays(t *testing.T) {
	h := health.New()
	// Seed with one error (rate = 1.0).
	h.RecordResult("http://b", false)
	// 9 successes → rate decays toward 0.
	for i := 0; i < 9; i++ {
		h.RecordResult("http://b", true)
	}
	r := h.ErrorRate("http://b")
	if r >= 0.5 {
		t.Errorf("rate should have decayed below 0.5 after 9 successes, got %v", r)
	}
	if r <= 0 {
		t.Errorf("rate should remain positive after only 10 samples, got %v", r)
	}
}

func TestEffectiveWeight_NilSafe(t *testing.T) {
	var h *health.BackendHealth
	if w := h.EffectiveWeight("http://b", 10); w != 10 {
		t.Errorf("nil health should return configured weight, got %d", w)
	}
}

func TestEffectiveWeight_Thresholds(t *testing.T) {
	tests := []struct {
		name       string
		errors     int // number of errors to seed (out of 10 total calls)
		successes  int
		configured int
		wantWeight int
	}{
		{"healthy", 0, 10, 10, 10},          // errorRate ~0 → full weight
		{"penalised", 3, 7, 10, 5},          // errorRate ~0.3 → half weight
		{"penalised_min1", 3, 7, 1, 1},      // half of 1 is 0, but min is 1
		{"excluded", 6, 4, 10, 0},           // errorRate ~0.6 → excluded
		{"zero_config_not_penalised", 3, 7, 0, 0}, // weight=0 stays 0
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := health.New()
			for i := 0; i < tt.errors; i++ {
				h.RecordResult("http://b", false)
			}
			for i := 0; i < tt.successes; i++ {
				h.RecordResult("http://b", true)
			}
			got := h.EffectiveWeight("http://b", tt.configured)
			if got != tt.wantWeight {
				t.Errorf("EffectiveWeight = %d, want %d (errorRate=%.3f)",
					got, tt.wantWeight, h.ErrorRate("http://b"))
			}
		})
	}
}

func TestAdjustedBackends_NilSafe(t *testing.T) {
	var h *health.BackendHealth
	backends := []service.Backend{{URL: "http://b", Weight: 10}}
	out := h.AdjustedBackends(backends)
	if out[0].Weight != 10 {
		t.Error("nil health should return original backends unchanged")
	}
}

func TestAdjustedBackends_ZeroWeightNotPenalised(t *testing.T) {
	h := health.New()
	// Seed many errors so any positive weight would be zeroed.
	for i := 0; i < 20; i++ {
		h.RecordResult("http://fallback", false)
	}
	backends := []service.Backend{{URL: "http://fallback", Weight: 0}}
	out := h.AdjustedBackends(backends)
	if out[0].Weight != 0 {
		t.Errorf("weight=0 backend should not be further penalised, got %d", out[0].Weight)
	}
}

func TestAdjustedBackends_HealthyBackendsUnchanged(t *testing.T) {
	h := health.New()
	backends := []service.Backend{
		{URL: "http://a", Weight: 5},
		{URL: "http://b", Weight: 3},
	}
	out := h.AdjustedBackends(backends)
	if out[0].Weight != 5 || out[1].Weight != 3 {
		t.Errorf("healthy backends should keep original weights, got %d %d", out[0].Weight, out[1].Weight)
	}
}

func TestAdjustedBackends_DoesNotMutateOriginal(t *testing.T) {
	h := health.New()
	for i := 0; i < 10; i++ {
		h.RecordResult("http://b", false)
	}
	backends := []service.Backend{{URL: "http://b", Weight: 10}}
	h.AdjustedBackends(backends)
	if backends[0].Weight != 10 {
		t.Error("AdjustedBackends must not mutate the original slice")
	}
}
