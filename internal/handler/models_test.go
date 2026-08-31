package handler_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"gatewai/gateway/internal/config"
	"gatewai/gateway/internal/handler"
	"gatewai/gateway/internal/service"
)

func callListModelsWithModel(t *testing.T, reg *service.Registry, modelName string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/models?model="+modelName, nil)
	w := httptest.NewRecorder()
	handler.ListModels(reg, "", nil)(w, req)
	return w
}

// modelsResponse mirrors the JSON structure returned by GET /v1/models.
type modelsResponse struct {
	Object string `json:"object"`
	Data   []struct {
		ID            string   `json:"id"`
		Object        string   `json:"object"`
		OwnedBy       string   `json:"owned_by"`
		ServiceType   string   `json:"service_type"`
		Provider      string   `json:"provider,omitempty"`
		BackendModel  string   `json:"backend_model,omitempty"`
		BackendModels []string `json:"backend_models,omitempty"`
		Capabilities  struct {
			SupportsAsync     bool     `json:"supports_async"`
			SupportsSync      bool     `json:"supports_sync"`
			SupportsStreaming bool     `json:"supports_streaming"`
			AcceptedFormats   []string `json:"accepted_formats,omitempty"`
			MaxFileSizeMB     int64    `json:"max_file_size_mb,omitempty"`
			Operations        []string `json:"operations,omitempty"`
			Deprecated        bool     `json:"deprecated,omitempty"`
			Degraded          bool     `json:"degraded,omitempty"`
		} `json:"capabilities"`
	} `json:"data"`
}

func callListModels(t *testing.T, reg *service.Registry) modelsResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	handler.ListModels(reg, "", nil)(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp modelsResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp
}

func TestListModels_Capabilities_AsyncOnly(t *testing.T) {
	reg := service.NewRegistry([]config.ServiceConfig{{
		Type:          "transcription",
		Model:         "whisper-large-v3",
		AcceptedExts:  []string{".mp3", ".wav"},
		MaxFileSizeMB: 100,
		Operations: map[string][]string{
			"transcription": {"/v1/audio/transcriptions"},
			"translation":   {"/v1/audio/translations"},
		},
	}})

	resp := callListModels(t, reg)

	if resp.Object != "list" {
		t.Errorf("expected object='list', got %q", resp.Object)
	}
	if len(resp.Data) != 1 {
		t.Fatalf("expected 1 model, got %d", len(resp.Data))
	}
	m := resp.Data[0]
	if m.ID != "whisper-large-v3" {
		t.Errorf("expected id='whisper-large-v3', got %q", m.ID)
	}
	if m.OwnedBy != "gatewai" {
		t.Errorf("expected owned_by='gatewai', got %q", m.OwnedBy)
	}
	if m.ServiceType != "transcription" {
		t.Errorf("expected service_type='transcription', got %q", m.ServiceType)
	}
	if !m.Capabilities.SupportsAsync {
		t.Error("expected supports_async=true for async file-upload service")
	}
	if m.Capabilities.SupportsSync {
		t.Error("expected supports_sync=false (no inference_url or backends)")
	}
	if m.Capabilities.SupportsStreaming {
		t.Error("expected supports_streaming=false (no provider)")
	}
	if m.Capabilities.MaxFileSizeMB != 100 {
		t.Errorf("expected max_file_size_mb=100, got %d", m.Capabilities.MaxFileSizeMB)
	}
	// Operations sorted alphabetically.
	if len(m.Capabilities.Operations) != 2 ||
		m.Capabilities.Operations[0] != "transcription" ||
		m.Capabilities.Operations[1] != "translation" {
		t.Errorf("unexpected operations: %v", m.Capabilities.Operations)
	}
	// AcceptedFormats sorted alphabetically.
	if len(m.Capabilities.AcceptedFormats) != 2 ||
		m.Capabilities.AcceptedFormats[0] != ".mp3" ||
		m.Capabilities.AcceptedFormats[1] != ".wav" {
		t.Errorf("unexpected accepted_formats: %v", m.Capabilities.AcceptedFormats)
	}
}

func TestListModels_Capabilities_Deprecated(t *testing.T) {
	reg := service.NewRegistry([]config.ServiceConfig{
		{
			Type:       "transcription",
			Model:      "whisper-large-v2",
			Deprecated: true,
		},
		{
			Type:  "transcription",
			Model: "whisper-large-v3",
		},
	})

	resp := callListModels(t, reg)
	if len(resp.Data) != 2 {
		t.Fatalf("expected 2 models, got %d", len(resp.Data))
	}
	for _, m := range resp.Data {
		switch m.ID {
		case "whisper-large-v2":
			if !m.Capabilities.Deprecated {
				t.Error("expected deprecated=true for whisper-large-v2")
			}
		case "whisper-large-v3":
			if m.Capabilities.Deprecated {
				t.Error("expected deprecated=false for whisper-large-v3")
			}
		}
	}
}

func TestListModels_Capabilities_SyncLLM(t *testing.T) {
	reg := service.NewRegistry([]config.ServiceConfig{{
		Type:     "llm",
		Model:    "gpt-4o",
		Provider: "openai",
		Operations: map[string][]string{
			"chat": {"/v1/chat/completions"},
		},
		InferenceURL: "http://backend.example.com",
	}})

	resp := callListModels(t, reg)

	if len(resp.Data) != 1 {
		t.Fatalf("expected 1 model, got %d", len(resp.Data))
	}
	m := resp.Data[0]
	if m.Provider != "openai" {
		t.Errorf("expected provider='openai', got %q", m.Provider)
	}
	if !m.Capabilities.SupportsSync {
		t.Error("expected supports_sync=true (has inference_url)")
	}
	if !m.Capabilities.SupportsStreaming {
		t.Error("expected supports_streaming=true (has provider)")
	}
	if m.Capabilities.SupportsAsync {
		t.Error("expected supports_async=false (no accepted_exts configured)")
	}
}

func TestListModels_MultipleModels_SortedByID(t *testing.T) {
	reg := service.NewRegistry([]config.ServiceConfig{
		{
			Type:         "llm",
			Model:        "zephyr-7b",
			Provider:     "openai",
			Operations:   map[string][]string{"chat": {"/v1/chat/completions"}},
			InferenceURL: "http://b1.example.com",
		},
		{
			Type:         "llm",
			Model:        "gpt-4o",
			Provider:     "openai",
			Operations:   map[string][]string{"chat": {"/v1/chat/completions"}},
			InferenceURL: "http://b2.example.com",
		},
		{
			Type:         "llm",
			Model:        "mistral-7b",
			Provider:     "openai",
			Operations:   map[string][]string{"chat": {"/v1/chat/completions"}},
			InferenceURL: "http://b3.example.com",
		},
	})

	resp := callListModels(t, reg)

	if len(resp.Data) != 3 {
		t.Fatalf("expected 3 models, got %d", len(resp.Data))
	}
	ids := []string{resp.Data[0].ID, resp.Data[1].ID, resp.Data[2].ID}
	want := []string{"gpt-4o", "mistral-7b", "zephyr-7b"}
	for i, got := range ids {
		if got != want[i] {
			t.Errorf("models[%d]: expected %q, got %q", i, want[i], got)
		}
	}
}

func TestListModels_Capabilities_MultiBackend(t *testing.T) {
	reg := service.NewRegistry([]config.ServiceConfig{{
		Type:       "llm",
		Model:      "llama3",
		Provider:   "ollama",
		Operations: map[string][]string{"chat": {"/v1/chat/completions"}},
		Backends: []config.BackendConfig{
			{URL: "http://b1.example.com", Weight: 100},
			{URL: "http://b2.example.com", Weight: 0},
		},
	}})

	resp := callListModels(t, reg)

	m := resp.Data[0]
	if !m.Capabilities.SupportsSync {
		t.Error("expected supports_sync=true (has backends)")
	}
	if !m.Capabilities.SupportsStreaming {
		t.Error("expected supports_streaming=true (has provider)")
	}
}

func TestListModels_ProxyModel_ForwardsToBackend(t *testing.T) {
	backendResp := `{"object":"list","data":[{"id":"gpt-4o","object":"model","context_length":128000}]}`
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(backendResp))
	}))
	defer backend.Close()

	reg := service.NewRegistry([]config.ServiceConfig{{
		Type:         "llm",
		Model:        "gpt-4o",
		Provider:     "openai",
		InferenceURL: backend.URL + "/v1/chat/completions",
		Operations:   map[string][]string{"chat": {"/v1/chat/completions"}},
	}})

	w := callListModelsWithModel(t, reg, "gpt-4o")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if w.Body.String() != backendResp {
		t.Errorf("unexpected body: %s", w.Body.String())
	}
}

func TestListModels_ProxyModel_NotFound(t *testing.T) {
	reg := service.NewRegistry([]config.ServiceConfig{{
		Type:  "llm",
		Model: "gpt-4o",
	}})

	w := callListModelsWithModel(t, reg, "unknown-model")

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestListModels_ProxyModel_NoBackend_NotFound(t *testing.T) {
	// Model exists but has no inference_url
	reg := service.NewRegistry([]config.ServiceConfig{{
		Type:         "transcription",
		Model:        "whisper-large-v3",
		AcceptedExts: []string{".mp3"},
		Operations:   map[string][]string{"transcription": {"/v1/audio/transcriptions"}},
	}})

	w := callListModelsWithModel(t, reg, "whisper-large-v3")

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for model without backend, got %d", w.Code)
	}
}

func TestListModels_EmptyRegistry_ReturnsEmptyList(t *testing.T) {
	// Registry always requires at least one service, but the handler must not panic.
	reg := service.NewRegistry([]config.ServiceConfig{{
		Type:         "transcription",
		Model:        "whisper",
		InferenceURL: "http://inference.example.com",
	}})

	resp := callListModels(t, reg)
	if resp.Object != "list" {
		t.Errorf("expected object='list', got %q", resp.Object)
	}
}

// ── Backend model exposure (feature 1) ─────────────────────────────────────────

func TestListModels_BackendModel_ServiceLevel(t *testing.T) {
	reg := service.NewRegistry([]config.ServiceConfig{{
		Type:         "llm",
		Model:        "gpt-4o",
		Provider:     "passthrough",
		BackendModel: "meta-llama/Meta-Llama-3-8B-Instruct",
		Operations:   map[string][]string{"chat": {"/v1/chat/completions"}},
		InferenceURL: "http://b.example.com",
	}})

	resp := callListModels(t, reg)
	m := resp.Data[0]
	if m.BackendModel != "meta-llama/Meta-Llama-3-8B-Instruct" {
		t.Errorf("expected backend_model exposed, got %q", m.BackendModel)
	}
	if len(m.BackendModels) != 0 {
		t.Errorf("expected no backend_models for single backend model, got %v", m.BackendModels)
	}
}

func TestListModels_BackendModel_DistinctPerBackend(t *testing.T) {
	reg := service.NewRegistry([]config.ServiceConfig{{
		Type:       "llm",
		Model:      "chat",
		Provider:   "passthrough",
		Operations: map[string][]string{"chat": {"/v1/chat/completions"}},
		Backends: []config.BackendConfig{
			{URL: "http://b1.example.com", Weight: 90, Model: "llama-3-8b"},
			{URL: "http://b2.example.com", Weight: 10, Model: "llama-3-70b"},
		},
	}})

	resp := callListModels(t, reg)
	m := resp.Data[0]
	if m.BackendModel != "llama-3-8b" {
		t.Errorf("expected primary backend_model='llama-3-8b', got %q", m.BackendModel)
	}
	if len(m.BackendModels) != 2 || m.BackendModels[0] != "llama-3-8b" || m.BackendModels[1] != "llama-3-70b" {
		t.Errorf("expected both backend models listed, got %v", m.BackendModels)
	}
}

func TestListModels_BackendModel_OmittedWhenUnset(t *testing.T) {
	reg := service.NewRegistry([]config.ServiceConfig{{
		Type:         "llm",
		Model:        "gpt-4o",
		Provider:     "openai",
		Operations:   map[string][]string{"chat": {"/v1/chat/completions"}},
		InferenceURL: "http://b.example.com",
	}})

	resp := callListModels(t, reg)
	m := resp.Data[0]
	if m.BackendModel != "" {
		t.Errorf("expected no backend_model when no rewrite configured, got %q", m.BackendModel)
	}
}

// ── Model visibility gating (feature 2) ────────────────────────────────────────

// listModelsWithHeader issues GET /v1/models with the given user_type header value
// and returns the decoded response. userTypeHeader configures which header the
// handler reads; pass "" to disable header-based audience resolution.
func listModelsWithHeader(t *testing.T, reg *service.Registry, userTypeHeader, headerName, headerValue string) modelsResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	if headerName != "" {
		req.Header.Set(headerName, headerValue)
	}
	w := httptest.NewRecorder()
	handler.ListModels(reg, userTypeHeader, nil)(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp modelsResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp
}

func TestListModels_Visibility_HidesRestrictedFromPublic(t *testing.T) {
	reg := service.NewRegistry([]config.ServiceConfig{
		{
			Type:         "llm",
			Model:        "gpt-4o",
			Provider:     "openai",
			Operations:   map[string][]string{"chat": {"/v1/chat/completions"}},
			InferenceURL: "http://b1.example.com",
		},
		{
			Type:         "llm",
			Model:        "gpt-5-beta",
			Provider:     "openai",
			Operations:   map[string][]string{"chat": {"/v1/chat/completions"}},
			InferenceURL: "http://b2.example.com",
			Visibility:   config.VisibilityConfig{UserTypes: []string{"beta"}},
		},
	})

	// No identity → restricted model hidden.
	resp := listModelsWithHeader(t, reg, "X-User-Type", "", "")
	if len(resp.Data) != 1 || resp.Data[0].ID != "gpt-4o" {
		t.Fatalf("expected only public model visible, got %+v", resp.Data)
	}
}

func TestListModels_Visibility_ShowsToMatchingUserType(t *testing.T) {
	reg := service.NewRegistry([]config.ServiceConfig{
		{
			Type:         "llm",
			Model:        "gpt-4o",
			Provider:     "openai",
			Operations:   map[string][]string{"chat": {"/v1/chat/completions"}},
			InferenceURL: "http://b1.example.com",
		},
		{
			Type:         "llm",
			Model:        "gpt-5-beta",
			Provider:     "openai",
			Operations:   map[string][]string{"chat": {"/v1/chat/completions"}},
			InferenceURL: "http://b2.example.com",
			Visibility:   config.VisibilityConfig{UserTypes: []string{"beta"}},
		},
	})

	resp := listModelsWithHeader(t, reg, "X-User-Type", "X-User-Type", "beta")
	if len(resp.Data) != 2 {
		t.Fatalf("expected both models visible to beta user, got %d: %+v", len(resp.Data), resp.Data)
	}
}

func TestListModels_ProxyModel_RestrictedHiddenAs404(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list"}`))
	}))
	defer backend.Close()

	reg := service.NewRegistry([]config.ServiceConfig{{
		Type:         "llm",
		Model:        "gpt-5-beta",
		Provider:     "openai",
		InferenceURL: backend.URL + "/v1/chat/completions",
		Operations:   map[string][]string{"chat": {"/v1/chat/completions"}},
		Visibility:   config.VisibilityConfig{UserTypes: []string{"beta"}},
	}})

	// Caller without the beta user type must get 404, not a proxied response.
	req := httptest.NewRequest(http.MethodGet, "/v1/models?model=gpt-5-beta", nil)
	w := httptest.NewRecorder()
	handler.ListModels(reg, "X-User-Type", nil)(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for hidden model, got %d: %s", w.Code, w.Body.String())
	}
}

// fakeHealth marks a fixed set of backend URLs as circuit-open.
type fakeHealth struct{ open map[string]bool }

func (f fakeHealth) IsOpen(url string) bool { return f.open[url] }

func TestListModels_Degraded_WhenAllBackendsOpen(t *testing.T) {
	reg := service.NewRegistry([]config.ServiceConfig{{
		Type: "llm", Model: "chat", Provider: "passthrough",
		Operations: map[string][]string{"chat": {"/v1/chat/completions"}},
		Backends: []config.BackendConfig{
			{URL: "http://b1", Weight: 100},
			{URL: "http://b2", Weight: 0},
		},
	}})

	// Both backends open → degraded.
	health := fakeHealth{open: map[string]bool{"http://b1": true, "http://b2": true}}
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	handler.ListModels(reg, "", health)(w, req)
	var resp modelsResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Data) != 1 || !resp.Data[0].Capabilities.Degraded {
		t.Fatalf("expected model to be degraded when all backends open: %+v", resp.Data)
	}
}

func TestListModels_NotDegraded_WhenOneBackendHealthy(t *testing.T) {
	reg := service.NewRegistry([]config.ServiceConfig{{
		Type: "llm", Model: "chat", Provider: "passthrough",
		Operations: map[string][]string{"chat": {"/v1/chat/completions"}},
		Backends: []config.BackendConfig{
			{URL: "http://b1", Weight: 100},
			{URL: "http://b2", Weight: 0},
		},
	}})

	// b1 open, b2 healthy → not degraded (a request can still succeed).
	health := fakeHealth{open: map[string]bool{"http://b1": true}}
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	handler.ListModels(reg, "", health)(w, req)
	var resp modelsResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Data) != 1 || resp.Data[0].Capabilities.Degraded {
		t.Fatalf("model should not be degraded when a backend is healthy: %+v", resp.Data)
	}
}

func TestListModels_NilHealth_NeverDegraded(t *testing.T) {
	reg := service.NewRegistry([]config.ServiceConfig{{
		Type: "llm", Model: "chat", Provider: "passthrough",
		Operations: map[string][]string{"chat": {"/v1/chat/completions"}},
		Backends:   []config.BackendConfig{{URL: "http://b1", Weight: 100}},
	}})
	resp := callListModels(t, reg) // passes nil health
	if len(resp.Data) != 1 || resp.Data[0].Capabilities.Degraded {
		t.Fatalf("nil health must never mark degraded: %+v", resp.Data)
	}
}
