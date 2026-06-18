package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"gatewai/gateway/internal/ratelimit"
)

// TestTokenChecker_NilStaysNilInterface guards against the typed-nil interface
// trap: passing a nil *ratelimit.Limiter as a ratelimit.TokenChecker would
// otherwise yield a non-nil interface, defeating the handler's nil check and
// panicking on first use (LLM proxy request with rate limiting disabled).
func TestTokenChecker_NilStaysNilInterface(t *testing.T) {
	var l *ratelimit.Limiter // nil
	if got := tokenChecker(l); got != nil {
		t.Fatalf("tokenChecker(nil) = non-nil interface %v, want nil", got)
	}
	real := ratelimit.New(nil, nil, nil, "", "")
	if got := tokenChecker(real); got == nil {
		t.Fatal("tokenChecker(non-nil) = nil, want the limiter")
	}
}

// TestReservedGatewayPath verifies the path guard against the full list of
// reserved gateway routes.
func TestReservedGatewayPath(t *testing.T) {
	cases := []struct {
		path     string
		reserved bool
	}{
		// Exact reserved paths
		{"/health", true},
		{"/metrics", true},
		{"/docs", true},
		{"/openapi.yaml", true},
		{"/jobs", true},
		// Sub-paths of reserved prefixes
		{"/docs/spec/audio/whisper", true},
		{"/docs/anything", true},
		{"/jobs/audio", true},
		{"/jobs/audio/123", true},
		{"/-/reload", true},
		{"/-/anything", true},
		// Non-reserved service paths
		{"/rerank", false},
		{"/v1/audio/transcriptions", false},
		{"/v1/audio/translations", false},
		{"/v1/chat/completions", false},
		{"/rerank", false},
		{"/v2/models/{model}/infer", false},
		{"/ocr", false},
		// Paths that share a prefix but are not reserved
		{"/healthz", false},
		{"/metrics-custom", false},
		{"/documents", false},
		{"/jobsboard", false},
	}

	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			got := reservedGatewayPath(tc.path)
			if got != tc.reserved {
				t.Errorf("reservedGatewayPath(%q) = %v, want %v", tc.path, got, tc.reserved)
			}
		})
	}
}

// TestReservedPathsNotOverriddenBySync verifies that when a service is
// configured with a reserved path (e.g. /health), the gateway's own handler
// still responds — the sync handler is never registered for that path.
func TestReservedPathsNotOverriddenBySync(t *testing.T) {
	gatewayCalled := false
	syncCalled := false

	gatewayHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gatewayCalled = true
		w.WriteHeader(http.StatusOK)
	})
	syncHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		syncCalled = true
		w.WriteHeader(http.StatusOK)
	})

	r := chi.NewRouter()
	// Simulate gateway reserved routes (GET on /health, POST on /jobs/{service_type}).
	r.Get("/health", gatewayHandler)
	r.Post("/jobs/{service_type}", gatewayHandler)

	// Simulate the guarded sync path registration loop.
	configuredPaths := []string{
		"/health",            // reserved — must be skipped
		"/jobs/audio",        // reserved — must be skipped
		"/v1/audio/transcriptions", // valid — must be registered
		"/rerank",            // valid — must be registered
	}
	for _, path := range configuredPaths {
		if reservedGatewayPath(path) {
			continue
		}
		r.Post(path, syncHandler)
	}

	t.Run("GET /health returns gateway handler", func(t *testing.T) {
		gatewayCalled, syncCalled = false, false
		req := httptest.NewRequest(http.MethodGet, "/health", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if !gatewayCalled {
			t.Error("expected gateway handler to be called for GET /health")
		}
		if syncCalled {
			t.Error("sync handler must not be called for GET /health")
		}
	})

	t.Run("POST /jobs/audio returns gateway handler", func(t *testing.T) {
		gatewayCalled, syncCalled = false, false
		req := httptest.NewRequest(http.MethodPost, "/jobs/audio", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if !gatewayCalled {
			t.Error("expected gateway handler to be called for POST /jobs/audio")
		}
		if syncCalled {
			t.Error("sync handler must not be called for POST /jobs/audio")
		}
	})

	t.Run("POST /v1/audio/transcriptions routes to sync handler", func(t *testing.T) {
		gatewayCalled, syncCalled = false, false
		req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if !syncCalled {
			t.Error("expected sync handler to be called for POST /v1/audio/transcriptions")
		}
	})

	t.Run("POST /rerank routes to sync handler", func(t *testing.T) {
		gatewayCalled, syncCalled = false, false
		req := httptest.NewRequest(http.MethodPost, "/rerank", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if !syncCalled {
			t.Error("expected sync handler to be called for POST /rerank")
		}
	})
}
