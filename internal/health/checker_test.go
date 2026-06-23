package health

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gatewai/gateway/internal/config"
	"gatewai/gateway/internal/service"
)

// probeVia is a test helper that creates a minimal Checker and probes d directly.
func probeVia(t *testing.T, d *service.Def, defaultTimeout string) string {
	t.Helper()
	cfg := config.HealthConfig{Timeout: defaultTimeout}
	c := &Checker{
		defaultTimeout: cfg.TimeoutDuration(),
		httpClient:     &http.Client{},
	}
	if c.defaultTimeout <= 0 {
		c.defaultTimeout = 5e9 // 5s
	}
	return c.probe(t.Context(), d)
}

func def(inferenceURL string, healthCfg config.ServiceHealthConfig) *service.Def {
	return &service.Def{
		Type:         "audio",
		Model:        "test-model",
		InferenceURL: inferenceURL,
		HealthCheck:  healthCfg,
	}
}

func TestProbe_Up_2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("expected HEAD, got %s", r.Method)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	got := probeVia(t, def(srv.URL, config.ServiceHealthConfig{}), "5s")
	if got != "up" {
		t.Errorf("expected up, got %s", got)
	}
}

func TestProbe_Down_5xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	got := probeVia(t, def(srv.URL, config.ServiceHealthConfig{}), "5s")
	if got != "down" {
		t.Errorf("expected down, got %s", got)
	}
}

func TestProbe_Down_ConnectionRefused(t *testing.T) {
	got := probeVia(t, def("http://127.0.0.1:1", config.ServiceHealthConfig{}), "200ms")
	if got != "down" {
		t.Errorf("expected down, got %s", got)
	}
}

func TestProbe_Up_4xx(t *testing.T) {
	// 404 means the backend is responding — treat as "up".
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	got := probeVia(t, def(srv.URL, config.ServiceHealthConfig{}), "5s")
	if got != "up" {
		t.Errorf("expected up for 404, got %s", got)
	}
}

func TestProbe_CustomPath(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	svcDef := def(srv.URL, config.ServiceHealthConfig{Path: "/readyz"})
	probeVia(t, svcDef, "5s")

	if gotPath != "/readyz" {
		t.Errorf("expected /readyz, got %s", gotPath)
	}
}

func TestProbe_ServiceTimeout_Overrides_Default(t *testing.T) {
	// Backend takes 150ms; service timeout is 50ms → should time out → "down".
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulate slow response by not responding promptly.
		// The test client will time out before we write.
		select {}
	}))
	defer slow.Close()

	svcDef := def(slow.URL, config.ServiceHealthConfig{Timeout: "50ms"})
	got := probeVia(t, svcDef, "10s") // global default is generous
	if got != "down" {
		t.Errorf("expected down on timeout, got %s", got)
	}
}

func TestProbeURL_DefaultPath(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	probeVia(t, def(srv.URL, config.ServiceHealthConfig{}), "5s")
	if gotPath != "/health" {
		t.Errorf("expected /health, got %s", gotPath)
	}
}

func TestProbeURL_TrailingSlashInInferenceURL(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// InferenceURL has a trailing slash — should not produce double slash.
	svcDef := def(srv.URL+"/", config.ServiceHealthConfig{})
	probeVia(t, svcDef, "5s")
	if strings.Contains(gotPath, "//") {
		t.Errorf("double slash in probe URL: %s", gotPath)
	}
}
