package handler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gatewai/gateway/internal/config"
	"gatewai/gateway/internal/handler"
	"gatewai/gateway/internal/health"
	"gatewai/gateway/internal/service"
)

// ── Health ────────────────────────────────────────────────────────────────────

func TestHealth_Returns200(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	handler.Health(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestHealth_ContentType(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	handler.Health(w, req)

	ct := w.Header().Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Errorf("expected application/json content-type, got %q", ct)
	}
}

func TestHealth_BodyHasStatusOK(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	handler.Health(w, req)

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf(`expected "status": "ok", got %v`, body["status"])
	}
	if _, ok := body["time"]; !ok {
		t.Error(`expected "time" field in health response`)
	}
}

// ── NewHealthHandler ──────────────────────────────────────────────────────────
// The handler reads from a cached snapshot — no live probing on the request path.
// Tests inject a snapshot function directly.

func snapshotFn(snap *health.Snapshot, err error) func(context.Context) (*health.Snapshot, error) {
	return func(_ context.Context) (*health.Snapshot, error) { return snap, err }
}

func TestNewHealthHandler_NoParams_LightweightResponse(t *testing.T) {
	h := handler.NewHealthHandler(snapshotFn(nil, nil))

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf(`expected "status": "ok", got %v`, body["status"])
	}
	if _, ok := body["time"]; !ok {
		t.Error(`expected "time" field`)
	}
}

func TestNewHealthHandler_NoSnapshot_ReturnsUnknown(t *testing.T) {
	h := handler.NewHealthHandler(snapshotFn(nil, nil))

	req := httptest.NewRequest(http.MethodGet, "/health?verbose=true", nil)
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if body["status"] != "unknown" {
		t.Errorf(`expected "status": "unknown" when no snapshot yet, got %v`, body["status"])
	}
}

func TestNewHealthHandler_NoSnapshot_Strict_Returns500(t *testing.T) {
	h := handler.NewHealthHandler(snapshotFn(nil, nil))

	req := httptest.NewRequest(http.MethodGet, "/health?verbose=true&mode=strict", nil)
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 when no snapshot in strict mode, got %d", w.Code)
	}
}

func TestNewHealthHandler_Verbose_AllUp(t *testing.T) {
	snap := &health.Snapshot{
		CheckedAt: time.Now(),
		Backends:  map[string]string{"whisper-large": "up", "llava": "up"},
	}
	h := handler.NewHealthHandler(snapshotFn(snap, nil))

	req := httptest.NewRequest(http.MethodGet, "/health?verbose=true", nil)
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if body["status"] != "up" {
		t.Errorf(`expected "status": "up", got %v`, body["status"])
	}
	backends, ok := body["backends"].(map[string]any)
	if !ok {
		t.Fatalf(`expected "backends" map, got %T`, body["backends"])
	}
	for model, s := range backends {
		if s != "up" {
			t.Errorf("model %q: expected up, got %v", model, s)
		}
	}
}

func TestNewHealthHandler_Verbose_OneDown_ReturnsPartial(t *testing.T) {
	snap := &health.Snapshot{
		CheckedAt: time.Now(),
		Backends:  map[string]string{"whisper-large": "up", "llava": "down"},
	}
	h := handler.NewHealthHandler(snapshotFn(snap, nil))

	req := httptest.NewRequest(http.MethodGet, "/health?verbose=true", nil)
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 (non-strict partial), got %d", w.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if body["status"] != "partial" {
		t.Errorf(`expected "status": "partial", got %v`, body["status"])
	}
}

func TestNewHealthHandler_Verbose_AllDown_ReturnsDown(t *testing.T) {
	snap := &health.Snapshot{
		CheckedAt: time.Now(),
		Backends:  map[string]string{"whisper-large": "down", "llava": "down"},
	}
	h := handler.NewHealthHandler(snapshotFn(snap, nil))

	req := httptest.NewRequest(http.MethodGet, "/health?verbose=true", nil)
	w := httptest.NewRecorder()
	h(w, req)

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if body["status"] != "down" {
		t.Errorf(`expected "status": "down", got %v`, body["status"])
	}
}

func TestNewHealthHandler_Strict_Partial_Returns500AndDown(t *testing.T) {
	snap := &health.Snapshot{
		CheckedAt: time.Now(),
		Backends:  map[string]string{"whisper-large": "up", "llava": "down"},
	}
	h := handler.NewHealthHandler(snapshotFn(snap, nil))

	req := httptest.NewRequest(http.MethodGet, "/health?verbose=true&mode=strict", nil)
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 (strict + partial), got %d", w.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if body["status"] != "down" {
		t.Errorf(`expected "status": "down" in strict mode for partial, got %v`, body["status"])
	}
}

func TestNewHealthHandler_EmptySnapshot_ReturnsUnknown(t *testing.T) {
	// Snapshot exists but no backends were probed (all disabled or no InferenceURL).
	snap := &health.Snapshot{
		CheckedAt: time.Now(),
		Backends:  map[string]string{},
	}
	h := handler.NewHealthHandler(snapshotFn(snap, nil))

	req := httptest.NewRequest(http.MethodGet, "/health?verbose=true", nil)
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if body["status"] != "unknown" {
		t.Errorf(`expected "status": "unknown" for empty backends, got %v`, body["status"])
	}
}

func TestNewHealthHandler_ModelFilter_Up(t *testing.T) {
	snap := &health.Snapshot{
		CheckedAt: time.Now(),
		Backends:  map[string]string{"whisper-large": "up", "llava": "down"},
	}
	h := handler.NewHealthHandler(snapshotFn(snap, nil))

	req := httptest.NewRequest(http.MethodGet, "/health?model=whisper-large", nil)
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if body["status"] != "up" {
		t.Errorf(`expected "status": "up" for whisper-large, got %v`, body["status"])
	}
	if _, ok := body["backends"]; ok {
		t.Error(`"backends" should not be present without verbose=true`)
	}
}

func TestNewHealthHandler_ModelFilter_Down_Strict(t *testing.T) {
	snap := &health.Snapshot{
		CheckedAt: time.Now(),
		Backends:  map[string]string{"whisper-large": "up", "llava": "down"},
	}
	h := handler.NewHealthHandler(snapshotFn(snap, nil))

	req := httptest.NewRequest(http.MethodGet, "/health?model=llava&mode=strict", nil)
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 for down model in strict mode, got %d", w.Code)
	}
}

func TestNewHealthHandler_CheckedAt_PresentInVerbose(t *testing.T) {
	snap := &health.Snapshot{
		CheckedAt: time.Now(),
		Backends:  map[string]string{"whisper-large": "up"},
	}
	h := handler.NewHealthHandler(snapshotFn(snap, nil))

	req := httptest.NewRequest(http.MethodGet, "/health?verbose=true", nil)
	w := httptest.NewRecorder()
	h(w, req)

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if _, ok := body["checked_at"]; !ok {
		t.Error(`expected "checked_at" in verbose response`)
	}
}

// ── Reload ────────────────────────────────────────────────────────────────────

func TestReload_Success(t *testing.T) {
	called := false
	h := handler.NewReloadHandler(func() error {
		called = true
		return nil
	})

	req := httptest.NewRequest(http.MethodPost, "/-/reload", nil)
	w := httptest.NewRecorder()
	h(w, req)

	if !called {
		t.Error("reload function should have been called")
	}
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestReload_Failure_Returns500(t *testing.T) {
	h := handler.NewReloadHandler(func() error {
		return fmt.Errorf("config file not found")
	})

	req := httptest.NewRequest(http.MethodPost, "/-/reload", nil)
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 on reload error, got %d", w.Code)
	}
}

func TestReload_Failure_BodyContainsError(t *testing.T) {
	h := handler.NewReloadHandler(func() error {
		return fmt.Errorf("disk full")
	})

	req := httptest.NewRequest(http.MethodPost, "/-/reload", nil)
	w := httptest.NewRecorder()
	h(w, req)

	if !strings.Contains(w.Body.String(), "disk full") {
		t.Errorf("expected error message in body, got: %s", w.Body.String())
	}
}

// ── ListModels ────────────────────────────────────────────────────────────────

func TestListModels_EmptyRegistry(t *testing.T) {
	reg := service.NewRegistry([]config.ServiceConfig{})
	h := handler.ListModels(reg)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if body["object"] != "list" {
		t.Errorf(`expected "object": "list", got %v`, body["object"])
	}
	data, _ := body["data"].([]any)
	if len(data) != 0 {
		t.Errorf("expected empty data array, got %d items", len(data))
	}
}

func TestListModels_ReturnsAllModels(t *testing.T) {
	reg := service.NewRegistry([]config.ServiceConfig{
		{
			Type: "transcription", Model: "whisper-large",
			Operations:   map[string][]string{"transcription": {"/v1/audio/transcriptions"}},
			InferenceURL: "http://svc",
		},
		{
			Type: "transcription", Model: "whisper-turbo",
			Operations:   map[string][]string{"transcription": {"/v1/audio/transcriptions"}},
			InferenceURL: "http://svc2",
		},
	})
	h := handler.ListModels(reg)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	h(w, req)

	var body struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if len(body.Data) != 2 {
		t.Errorf("expected 2 models, got %d", len(body.Data))
	}
	for _, m := range body.Data {
		if m.Object != "model" {
			t.Errorf(`expected object "model", got %q`, m.Object)
		}
		if m.OwnedBy != "gatewai" {
			t.Errorf(`expected owned_by "gatewai", got %q`, m.OwnedBy)
		}
	}
}

func TestListModels_SortedAlphabetically(t *testing.T) {
	reg := service.NewRegistry([]config.ServiceConfig{
		{Type: "t", Model: "zzz-model", Operations: map[string][]string{"op": {"/v1/p"}}, InferenceURL: "http://svc"},
		{Type: "t", Model: "aaa-model", Operations: map[string][]string{"op": {"/v1/p"}}, InferenceURL: "http://svc"},
		{Type: "t", Model: "mmm-model", Operations: map[string][]string{"op": {"/v1/p"}}, InferenceURL: "http://svc"},
	})
	h := handler.ListModels(reg)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	h(w, req)

	var body struct {
		Data []struct{ ID string `json:"id"` } `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if len(body.Data) != 3 {
		t.Fatalf("expected 3 models, got %d", len(body.Data))
	}
	if body.Data[0].ID != "aaa-model" || body.Data[1].ID != "mmm-model" || body.Data[2].ID != "zzz-model" {
		t.Errorf("models not sorted: got %v, %v, %v", body.Data[0].ID, body.Data[1].ID, body.Data[2].ID)
	}
}

func TestListModels_ContentType(t *testing.T) {
	reg := service.NewRegistry([]config.ServiceConfig{})
	h := handler.ListModels(reg)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	h(w, req)

	ct := w.Header().Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Errorf("expected application/json, got %q", ct)
	}
}
