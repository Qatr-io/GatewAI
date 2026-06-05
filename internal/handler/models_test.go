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

// modelsResponse mirrors the JSON structure returned by GET /v1/models.
type modelsResponse struct {
	Object string `json:"object"`
	Data   []struct {
		ID          string `json:"id"`
		Object      string `json:"object"`
		OwnedBy     string `json:"owned_by"`
		ServiceType string `json:"service_type"`
		Provider    string `json:"provider,omitempty"`
		Capabilities struct {
			SupportsAsync    bool     `json:"supports_async"`
			SupportsSync     bool     `json:"supports_sync"`
			SupportsStreaming bool     `json:"supports_streaming"`
			AcceptedFormats  []string `json:"accepted_formats,omitempty"`
			MaxFileSizeMB    int64    `json:"max_file_size_mb,omitempty"`
			Operations       []string `json:"operations,omitempty"`
		} `json:"capabilities"`
	} `json:"data"`
}

func callListModels(t *testing.T, reg *service.Registry) modelsResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	handler.ListModels(reg)(w, req)

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
	if m.OwnedBy != "kevent" {
		t.Errorf("expected owned_by='kevent', got %q", m.OwnedBy)
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
			Type:     "llm",
			Model:    "zephyr-7b",
			Provider: "openai",
			Operations: map[string][]string{"chat": {"/v1/chat/completions"}},
			InferenceURL: "http://b1.example.com",
		},
		{
			Type:     "llm",
			Model:    "gpt-4o",
			Provider: "openai",
			Operations: map[string][]string{"chat": {"/v1/chat/completions"}},
			InferenceURL: "http://b2.example.com",
		},
		{
			Type:     "llm",
			Model:    "mistral-7b",
			Provider: "openai",
			Operations: map[string][]string{"chat": {"/v1/chat/completions"}},
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
		Type:     "llm",
		Model:    "llama3",
		Provider: "ollama",
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
