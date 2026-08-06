package handler

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"gatewai/gateway/internal/config"
)

//go:embed testdata/whisper-api-openapi.json
var whisperFixture []byte

// serveFixture starts a test HTTP server returning the given body with the given status.
func serveFixture(t *testing.T, status int, body []byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
}

func TestFetchSwaggerSpecs_Success(t *testing.T) {
	srv := serveFixture(t, http.StatusOK, whisperFixture)
	defer srv.Close()

	cfgs := []config.ServiceConfig{
		{Type: "audio", Model: "whisper-large-v3", SwaggerURL: srv.URL},
	}

	specs := FetchSwaggerSpecs(cfgs)

	if len(specs) != 1 {
		t.Fatalf("expected 1 spec, got %d", len(specs))
	}
	if specs[0].Type != "audio" || specs[0].Model != "whisper-large-v3" {
		t.Errorf("unexpected spec identity: %s/%s", specs[0].Type, specs[0].Model)
	}
	if !json.Valid(specs[0].Data) {
		t.Error("spec data is not valid JSON")
	}
}

func TestFetchSwaggerSpecs_NoURL(t *testing.T) {
	cfgs := []config.ServiceConfig{
		{Type: "audio", Model: "whisper-large-v3"}, // no SwaggerURL
	}
	specs := FetchSwaggerSpecs(cfgs)
	if len(specs) != 0 {
		t.Errorf("expected 0 specs for service without swagger_url, got %d", len(specs))
	}
}

func TestFetchSwaggerSpecs_HTTP404(t *testing.T) {
	srv := serveFixture(t, http.StatusNotFound, []byte("not found"))
	defer srv.Close()

	cfgs := []config.ServiceConfig{
		{Type: "audio", Model: "whisper-large-v3", SwaggerURL: srv.URL},
	}
	specs := FetchSwaggerSpecs(cfgs)
	if len(specs) != 0 {
		t.Errorf("expected 0 specs on 404, got %d", len(specs))
	}
}

func TestFetchSwaggerSpecs_InvalidJSON(t *testing.T) {
	srv := serveFixture(t, http.StatusOK, []byte("not json at all"))
	defer srv.Close()

	cfgs := []config.ServiceConfig{
		{Type: "audio", Model: "whisper-large-v3", SwaggerURL: srv.URL},
	}
	specs := FetchSwaggerSpecs(cfgs)
	if len(specs) != 0 {
		t.Errorf("expected 0 specs on invalid JSON, got %d", len(specs))
	}
}

func TestFetchSwaggerSpecs_PartialFailure(t *testing.T) {
	good := serveFixture(t, http.StatusOK, whisperFixture)
	defer good.Close()
	bad := serveFixture(t, http.StatusInternalServerError, []byte("error"))
	defer bad.Close()

	cfgs := []config.ServiceConfig{
		{Type: "audio", Model: "whisper-large-v3", SwaggerURL: good.URL},
		{Type: "ocr", Model: "deepseek-ocr", SwaggerURL: bad.URL},
	}
	specs := FetchSwaggerSpecs(cfgs)
	if len(specs) != 1 {
		t.Fatalf("expected 1 spec (partial failure), got %d", len(specs))
	}
	if specs[0].Type != "audio" {
		t.Errorf("expected audio spec, got %s", specs[0].Type)
	}
}

func TestNewSwaggerHandler_Found(t *testing.T) {
	specs := []SwaggerSpec{
		{Type: "audio", Model: "whisper-large-v3", Data: whisperFixture},
	}

	r := chi.NewRouter()
	r.Get("/swagger/{type}/{model}", NewSwaggerHandler(specs))

	req := httptest.NewRequest(http.MethodGet, "/swagger/audio/whisper-large-v3", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("expected application/json, got %q", ct)
	}
	if !json.Valid(w.Body.Bytes()) {
		t.Error("response body is not valid JSON")
	}
}

func TestNewSwaggerHandler_NotFound(t *testing.T) {
	r := chi.NewRouter()
	r.Get("/swagger/{type}/{model}", NewSwaggerHandler(nil))

	req := httptest.NewRequest(http.MethodGet, "/swagger/audio/unknown-model", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestDocsUI_ContainsSwaggerURLs(t *testing.T) {
	specs := []SwaggerSpec{
		{Type: "audio", Model: "whisper-large-v3", Data: whisperFixture},
	}

	req := httptest.NewRequest(http.MethodGet, "/docs", nil)
	w := httptest.NewRecorder()
	DocsUI(specs)(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !contains(body, "/docs/spec/audio/whisper-large-v3") {
		t.Error("expected /docs/spec URL for whisper in docs HTML")
	}
	if !contains(body, "/openapi.yaml") {
		t.Error("expected gateway openapi.yaml URL in docs HTML")
	}
	if !contains(body, "audio / whisper-large-v3") {
		t.Error("expected service label in docs HTML")
	}
}

func TestDocsUI_NoSpecs(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/docs", nil)
	w := httptest.NewRecorder()
	DocsUI(nil)(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !contains(w.Body.String(), "/openapi.yaml") {
		t.Error("expected gateway openapi.yaml URL even without service specs")
	}
}

// ── ApplyGatewayOverlay ────────────────────────────────────────────────────────

func whisperSvc(ops map[string][]string) config.ServiceConfig {
	return config.ServiceConfig{
		Type:       "audio",
		Model:      "whisper-large-v3",
		Operations: ops,
	}
}

func getProperties(t *testing.T, spec json.RawMessage, path, method string) map[string]any {
	t.Helper()
	var s map[string]any
	if err := json.Unmarshal(spec, &s); err != nil {
		t.Fatalf("unmarshal spec: %v", err)
	}
	paths, _ := s["paths"].(map[string]any)
	item, _ := paths[path].(map[string]any)
	op, _ := item[method].(map[string]any)
	rb, _ := op["requestBody"].(map[string]any)
	content, _ := rb["content"].(map[string]any)
	for _, ct := range content {
		ctMap, _ := ct.(map[string]any)
		schema, _ := ctMap["schema"].(map[string]any)
		if props, ok := schema["properties"].(map[string]any); ok {
			return props
		}
		// $ref: resolve from components/schemas
		if ref, ok := schema["$ref"].(string); ok {
			const prefix = "#/components/schemas/"
			name := ref[len(prefix):]
			comps, _ := s["components"].(map[string]any)
			schemas, _ := comps["schemas"].(map[string]any)
			resolved, _ := schemas[name].(map[string]any)
			props, _ := resolved["properties"].(map[string]any)
			return props
		}
	}
	return nil
}

func TestApplyGatewayOverlay_SingleModel_SingleOp(t *testing.T) {
	svc := whisperSvc(map[string][]string{
		"transcription": {"/v1/audio/transcriptions"},
	})
	result := ApplyGatewayOverlay(whisperFixture, svc, []string{"whisper-large-v3"})

	props := getProperties(t, result, "/v1/audio/transcriptions", "post")
	if props == nil {
		t.Fatal("expected properties, got nil")
	}
	// model is always injected with default so it appears pre-filled in Swagger UI
	modelField, ok := props["model"].(map[string]any)
	if !ok {
		t.Fatal("expected model field to be injected")
	}
	if modelField["default"] != "whisper-large-v3" {
		t.Errorf("expected default=whisper-large-v3, got %v", modelField["default"])
	}
	// operation is not injected for single-operation service
	if _, ok := props["operation"]; ok {
		t.Error("operation field should not be injected for single-operation service")
	}
	if _, ok := props["file"]; !ok {
		t.Error("original 'file' field must still be present after overlay")
	}
}

func TestApplyGatewayOverlay_MultiModel_InjectsModelField(t *testing.T) {
	svc := whisperSvc(map[string][]string{
		"transcription": {"/v1/audio/transcriptions"},
	})
	// Two models exist for this type, but the spec is for whisper-large-v3 only.
	models := []string{"whisper-large-v3", "whisper-large-v3-turbo"}
	result := ApplyGatewayOverlay(whisperFixture, svc, models)

	props := getProperties(t, result, "/v1/audio/transcriptions", "post")
	if props == nil {
		t.Fatal("expected properties, got nil")
	}
	modelField, ok := props["model"].(map[string]any)
	if !ok {
		t.Fatal("expected model field to be injected")
	}
	enum, _ := modelField["enum"].([]any)
	if len(enum) != 1 {
		t.Errorf("expected 1 enum value (this model only), got %d", len(enum))
	}
	if enum[0] != "whisper-large-v3" {
		t.Errorf("expected enum[0] = whisper-large-v3, got %v", enum[0])
	}
}

func TestApplyGatewayOverlay_MultiOp_InjectsOperationField(t *testing.T) {
	svc := whisperSvc(map[string][]string{
		"transcription": {"/v1/audio/transcriptions"},
		"translation":   {"/v1/audio/translations"},
	})
	result := ApplyGatewayOverlay(whisperFixture, svc, []string{"whisper-large-v3"})

	props := getProperties(t, result, "/v1/audio/transcriptions", "post")
	if props == nil {
		t.Fatal("expected properties, got nil")
	}
	opField, ok := props["operation"].(map[string]any)
	if !ok {
		t.Fatal("expected operation field to be injected")
	}
	enum, _ := opField["enum"].([]any)
	if len(enum) != 2 {
		t.Errorf("expected 2 enum values, got %d", len(enum))
	}
}

func TestApplyGatewayOverlay_FiltersUnconfiguredPaths(t *testing.T) {
	svc := whisperSvc(map[string][]string{
		"transcription": {"/v1/audio/transcriptions"},
		// /v1/audio/translations is NOT in Operations
	})
	result := ApplyGatewayOverlay(whisperFixture, svc, []string{"whisper-large-v3"})

	var spec map[string]any
	if err := json.Unmarshal(result, &spec); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	paths, _ := spec["paths"].(map[string]any)

	if _, ok := paths["/v1/audio/translations"]; ok {
		t.Error("/v1/audio/translations must be removed: not declared in service operations")
	}
	if _, ok := paths["/v1/audio/transcriptions"]; !ok {
		t.Error("/v1/audio/transcriptions must be kept: declared in service operations")
	}
	// /v1/models is always injected by the overlay to document the gateway proxy feature.
	if _, ok := paths["/v1/models"]; !ok {
		t.Error("/v1/models must always be present in per-model swagger")
	}
}

func TestApplyGatewayOverlay_InjectsModelsEndpointWithDefault(t *testing.T) {
	svc := whisperSvc(map[string][]string{
		"transcription": {"/v1/audio/transcriptions"},
	})
	result := ApplyGatewayOverlay(whisperFixture, svc, []string{"whisper-large-v3"})

	var spec map[string]any
	if err := json.Unmarshal(result, &spec); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	paths, _ := spec["paths"].(map[string]any)
	item, _ := paths["/v1/models"].(map[string]any)
	get, _ := item["get"].(map[string]any)
	if get == nil {
		t.Fatal("expected GET operation on /v1/models")
	}
	params, _ := get["parameters"].([]any)
	if len(params) != 1 {
		t.Fatalf("expected 1 parameter, got %d", len(params))
	}
	param, _ := params[0].(map[string]any)
	if param["name"] != "model" || param["in"] != "query" {
		t.Errorf("unexpected parameter: %v", param)
	}
	schema, _ := param["schema"].(map[string]any)
	if schema["default"] != "whisper-large-v3" {
		t.Errorf("expected default=whisper-large-v3, got %v", schema["default"])
	}
	enum, _ := schema["enum"].([]any)
	if len(enum) != 1 || enum[0] != "whisper-large-v3" {
		t.Errorf("expected enum=[whisper-large-v3], got %v", enum)
	}
}

func TestApplyGatewayOverlay_Deprecated_MarksOperations(t *testing.T) {
	svc := whisperSvc(map[string][]string{
		"transcription": {"/v1/audio/transcriptions"},
	})
	svc.Deprecated = true
	result := ApplyGatewayOverlay(whisperFixture, svc, []string{"whisper-large-v3"})

	var spec map[string]any
	if err := json.Unmarshal(result, &spec); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	paths, _ := spec["paths"].(map[string]any)

	item, _ := paths["/v1/audio/transcriptions"].(map[string]any)
	op, _ := item["post"].(map[string]any)
	if op["deprecated"] != true {
		t.Errorf("expected deprecated=true on POST /v1/audio/transcriptions, got %v", op["deprecated"])
	}

	modelsItem, _ := paths["/v1/models"].(map[string]any)
	modelsGet, _ := modelsItem["get"].(map[string]any)
	if modelsGet["deprecated"] != true {
		t.Errorf("expected deprecated=true on GET /v1/models, got %v", modelsGet["deprecated"])
	}
}

func TestApplyGatewayOverlay_NotDeprecated_NoDeprecatedField(t *testing.T) {
	svc := whisperSvc(map[string][]string{
		"transcription": {"/v1/audio/transcriptions"},
	})
	result := ApplyGatewayOverlay(whisperFixture, svc, []string{"whisper-large-v3"})

	var spec map[string]any
	if err := json.Unmarshal(result, &spec); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	paths, _ := spec["paths"].(map[string]any)
	item, _ := paths["/v1/audio/transcriptions"].(map[string]any)
	op, _ := item["post"].(map[string]any)
	if _, ok := op["deprecated"]; ok {
		t.Errorf("expected no deprecated field when svc.Deprecated is false, got %v", op["deprecated"])
	}
}

func TestApplyGatewayOverlay_InvalidJSON_ReturnsUnchanged(t *testing.T) {
	bad := json.RawMessage(`not json`)
	result := ApplyGatewayOverlay(bad, whisperSvc(nil), []string{"whisper-large-v3"})
	if string(result) != string(bad) {
		t.Error("invalid input should be returned unchanged")
	}
}

func TestFetchSwaggerSpecs_OverlayApplied(t *testing.T) {
	srv := serveFixture(t, http.StatusOK, whisperFixture)
	defer srv.Close()

	cfgs := []config.ServiceConfig{
		{
			Type:  "audio",
			Model: "whisper-large-v3",
			Operations: map[string][]string{
				"transcription": {"/v1/audio/transcriptions"},
				"translation":   {"/v1/audio/translations"},
			},
			SwaggerURL: srv.URL,
		},
		{
			Type:       "audio",
			Model:      "whisper-large-v3-turbo",
			Operations: map[string][]string{"transcription": {"/v1/audio/transcriptions"}},
			SwaggerURL: srv.URL,
		},
	}

	specs := FetchSwaggerSpecs(cfgs)
	if len(specs) != 2 {
		t.Fatalf("expected 2 specs, got %d", len(specs))
	}

	// Both specs should have model field injected with their respective default values.
	for _, spec := range specs {
		props := getProperties(t, spec.Data, "/v1/audio/transcriptions", "post")
		if props == nil {
			t.Fatalf("nil properties for spec %s/%s", spec.Type, spec.Model)
		}
		modelField, ok := props["model"].(map[string]any)
		if !ok {
			t.Fatalf("model field not injected in spec %s/%s", spec.Type, spec.Model)
		}
		if modelField["default"] != spec.Model {
			t.Errorf("spec %s/%s: expected default=%s, got %v", spec.Type, spec.Model, spec.Model, modelField["default"])
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && containsStr(s, sub))
}

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
