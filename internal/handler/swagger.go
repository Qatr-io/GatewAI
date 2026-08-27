package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"gatewai/gateway/internal/config"
)

// SwaggerSpec holds a fetched OpenAPI spec for one service.
type SwaggerSpec struct {
	Type  string
	Model string
	Data  json.RawMessage
}

// key returns the lookup key used to index and serve the spec.
func (s SwaggerSpec) key() string { return s.Type + "/" + s.Model }

// FetchSwaggerSpecs fetches OpenAPI specs from each service's swagger_url field,
// then enriches each spec with gateway-owned fields (model, operation) via overlay.
// Failures are logged as warnings and skipped — they never block startup.
func FetchSwaggerSpecs(cfgs []config.ServiceConfig) []SwaggerSpec {
	// Pre-compute all models per service type so the overlay can build enums.
	modelsByType := map[string][]string{}
	for _, svc := range cfgs {
		modelsByType[svc.Type] = appendUniq(modelsByType[svc.Type], svc.Model)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	var specs []SwaggerSpec
	for _, svc := range cfgs {
		if svc.SwaggerURL == "" {
			continue
		}
		data, err := fetchSwaggerJSON(client, svc.SwaggerURL, svc.SwaggerHeaders)
		if err != nil {
			slog.Warn("failed to fetch swagger spec",
				"type", svc.Type, "model", svc.Model,
				"url", svc.SwaggerURL, "error", err)
			continue
		}
		data = ApplyGatewayOverlay(data, svc, modelsByType[svc.Type])
		specs = append(specs, SwaggerSpec{Type: svc.Type, Model: svc.Model, Data: data})
		slog.Info("swagger spec loaded", "type", svc.Type, "model", svc.Model)
	}
	return specs
}

// ApplyGatewayOverlay injects gateway-owned fields (model, operation) into the
// request body schemas of paths that match this service's configured operations.
// It resolves $ref pointers into components/schemas when needed.
// The original spec is returned unchanged on any parse error.
func ApplyGatewayOverlay(raw json.RawMessage, svc config.ServiceConfig, allModels []string) json.RawMessage {
	var spec map[string]any
	if err := json.Unmarshal(raw, &spec); err != nil {
		return raw
	}

	paths, _ := spec["paths"].(map[string]any)
	if paths == nil {
		return raw
	}

	managedPaths := map[string]bool{}
	for _, opPaths := range svc.Operations {
		for _, p := range opPaths {
			managedPaths[p] = true
		}
	}

	opNames := make([]string, 0, len(svc.Operations))
	for n := range svc.Operations {
		opNames = append(opNames, n)
	}
	sort.Strings(opNames)
	sort.Strings(allModels)

	for path, item := range paths {
		if !managedPaths[path] {
			delete(paths, path)
			continue
		}
		overlayPathItem(item, spec, svc.Model, opNames, svc.Deprecated)
	}

	// Always inject /v1/models GET with model query param so the per-model swagger
	// documents the gateway's proxy feature with the correct default model value.
	paths["/v1/models"] = modelsPathItemForModel(svc.Model, svc.Deprecated)

	out, err := json.Marshal(spec)
	if err != nil {
		return raw
	}
	return out
}

func modelsPathItemForModel(model string, deprecated bool) map[string]any {
	op := map[string]any{
		"summary": "Get model information",
		"description": "Proxied by the gateway to the underlying model backend. " +
			"Returns the model's native information (context size, capabilities, etc.).",
		"operationId": "getModelInfo_" + model,
		"parameters": []any{
			map[string]any{
				"name":        "model",
				"in":          "query",
				"required":    false,
				"description": "Model name. Defaults to this model.",
				"schema": map[string]any{
					"type":    "string",
					"default": model,
					"enum":    []string{model},
				},
			},
		},
		"responses": map[string]any{
			"200": map[string]any{
				"description": "Model information from the backend",
				"content": map[string]any{
					"application/json": map[string]any{
						"schema": map[string]any{"type": "object"},
					},
				},
			},
			"404": map[string]any{"description": "Model not found or has no backend"},
			"502": map[string]any{"description": "Backend unreachable"},
		},
	}
	if deprecated {
		op["deprecated"] = true
	}
	return map[string]any{"get": op}
}

func overlayPathItem(pathItem any, spec map[string]any, model string, opNames []string, deprecated bool) {
	item, _ := pathItem.(map[string]any)
	for _, method := range []string{"post", "put", "patch"} {
		op, _ := item[method].(map[string]any)
		if op == nil {
			continue
		}
		props := resolveRequestBodyProperties(op, spec)
		if props == nil {
			continue
		}
		props["model"] = map[string]any{
			"type":        "string",
			"enum":        []string{model},
			"default":     model,
			"description": "Inference model",
		}
		if len(opNames) > 1 {
			props["operation"] = map[string]any{
				"type":        "string",
				"enum":        opNames,
				"description": "Operation to perform — required when the model supports multiple operations",
			}
		}
		if deprecated {
			op["deprecated"] = true
		}
	}
}

// resolveRequestBodyProperties navigates requestBody → content → schema and
// returns the properties map, following a $ref into components/schemas if needed.
func resolveRequestBodyProperties(op map[string]any, spec map[string]any) map[string]any {
	rb, _ := op["requestBody"].(map[string]any)
	content, _ := rb["content"].(map[string]any)
	for _, ct := range content {
		ctMap, _ := ct.(map[string]any)
		schema, _ := ctMap["schema"].(map[string]any)
		if schema == nil {
			continue
		}
		if props, ok := schema["properties"].(map[string]any); ok {
			return props
		}
		if ref, ok := schema["$ref"].(string); ok {
			return resolveSchemaRef(ref, spec)
		}
	}
	return nil
}

// resolveSchemaRef resolves a local JSON reference (#/components/schemas/Foo)
// and returns the schema's properties map, or nil if unresolvable.
func resolveSchemaRef(ref string, spec map[string]any) map[string]any {
	const prefix = "#/components/schemas/"
	if !strings.HasPrefix(ref, prefix) {
		return nil
	}
	name := ref[len(prefix):]
	components, _ := spec["components"].(map[string]any)
	schemas, _ := components["schemas"].(map[string]any)
	schema, _ := schemas[name].(map[string]any)
	props, _ := schema["properties"].(map[string]any)
	return props
}

func fetchSwaggerJSON(client *http.Client, url string, headers map[string]string) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20)) // 4 MB limit
	if err != nil {
		return nil, err
	}
	if !json.Valid(data) {
		return nil, fmt.Errorf("response is not valid JSON")
	}
	return json.RawMessage(data), nil
}

// NewSwaggerHandler returns a handler that serves cached service OpenAPI specs.
// Route: GET /swagger/{type}/{model}
func NewSwaggerHandler(specs []SwaggerSpec) http.HandlerFunc {
	index := make(map[string]json.RawMessage, len(specs))
	for _, s := range specs {
		index[s.key()] = s.Data
	}
	return func(w http.ResponseWriter, r *http.Request) {
		key := chi.URLParam(r, "type") + "/" + chi.URLParam(r, "model")
		data, ok := index[key]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	}
}
