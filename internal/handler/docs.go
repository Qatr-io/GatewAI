package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"gatewai/gateway/internal/service"
)

// GenerateSpec builds an OpenAPI 3.0.3 spec dynamically from the service registry.
// Call once at startup after the registry is initialised; serve the result statically.
func GenerateSpec(reg *service.Registry, appVersion string) []byte {
	paths := map[string]any{}

	// ── Fixed endpoints ────────────────────────────────────────────────────────
	paths["/health"] = healthPathItem()

	serviceTypes := reg.Types()
	sort.Strings(serviceTypes)

	paths["/jobs/{service_type}"] = submitJobPathItem(serviceTypes, reg)
	paths["/jobs/{service_type}/{id}"] = jobByIDPathItem(serviceTypes)
	paths["/-/jobs/purge"] = purgeJobsPathItem()

	if reg.HasSyncServices() {
		paths["/v1/models"] = listModelsPathItem()
	}

	spec := map[string]any{
		"openapi": "3.0.3",
		"info": map[string]any{
			"title":   "Kevent Inference Gateway",
			"version": appVersion,
			"description": "Gateway for asynchronous and synchronous inference jobs.\n\n" +
				"**Async mode** — submit a file, get a `job_id`, poll for result or receive a webhook.\n\n" +
				"**Sync mode** — OpenAI-compatible endpoints, response held open until inference is done.",
		},
		"servers": []any{map[string]any{"url": "/"}},
		"tags":    specTags(reg),
		// Global security: all endpoints require the API key unless overridden.
		"security": []any{
			map[string]any{"ApiKeyAuth": []any{}},
		},
		"paths":      paths,
		"components": specComponents(),
	}

	out, _ := yaml.Marshal(spec)
	return out
}

// specTags builds the OpenAPI tags array for the gateway spec.
func specTags(_ *service.Registry) []any {
	return []any{
		map[string]any{"name": "Jobs", "description": "Async job submission and status"},
	}
}

// NewDocsSpec returns a handler that serves the pre-generated OpenAPI spec.
func NewDocsSpec(spec []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write(spec)
	}
}

// DocsUI serves the Swagger UI at GET /docs.
// Service specs are served under /docs/spec/{type}/{model}, which stays within
// the /docs* path prefix exposed by the API gateway.
func DocsUI(specs []SwaggerSpec) http.HandlerFunc {
	type urlEntry struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	}
	entries := []urlEntry{
		{Name: "Gateway (jobs async + sync)", URL: "/openapi.yaml"},
	}
	for _, s := range specs {
		entries = append(entries, urlEntry{
			Name: s.Type + " / " + s.Model,
			URL:  "/docs/spec/" + s.Type + "/" + s.Model,
		})
	}
	urlsJSON, _ := json.Marshal(entries)

	html := `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>Kevent API</title>
  <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist/swagger-ui.css" />
</head>
<body>
  <div id="swagger-ui"></div>
  <script src="https://unpkg.com/swagger-ui-dist/swagger-ui-bundle.js"></script>
  <script src="https://unpkg.com/swagger-ui-dist/swagger-ui-standalone-preset.js"></script>
  <script>
    SwaggerUIBundle({
      urls: ` + string(urlsJSON) + `,
      "urls.primaryName": "Gateway (jobs async + sync)",
      dom_id: "#swagger-ui",
      presets: [SwaggerUIBundle.presets.apis, SwaggerUIStandalonePreset],
      layout: "StandaloneLayout",
      deepLinking: true,
    });
  </script>
</body>
</html>`

	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(html))
	}
}

// ── Path item builders ─────────────────────────────────────────────────────────

func healthPathItem() map[string]any {
	statusSchema := map[string]any{
		"type": "string",
		"enum": []any{"ok", "up", "partial", "down", "unknown"},
	}
	backendsSchema := map[string]any{
		"type":                 "object",
		"additionalProperties": map[string]any{"type": "string", "enum": []any{"up", "down", "unknown"}},
		"description":          "Per-model status (only present when verbose=true)",
	}
	verboseResponseSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"status":     statusSchema,
			"checked_at": map[string]any{"type": "string", "format": "date-time"},
			"backends":   backendsSchema,
		},
	}
	simpleResponseSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"status": map[string]any{"type": "string", "example": "ok"},
			"time":   map[string]any{"type": "string", "format": "date-time"},
		},
	}
	errorResponseSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"status": statusSchema,
			"error":  map[string]any{"type": "string"},
		},
	}

	return map[string]any{
		"get": map[string]any{
			"summary":     "Health check",
			"operationId": "healthCheck",
			"security":    []any{}, // public — no API key required
			"description": "Without query params returns a lightweight `{\"status\":\"ok\"}` with no backend probing.\n\n" +
				"Add `?verbose=true` or `?model=<name>` to retrieve the cached per-backend probe results " +
				"(populated by the background health-check loop).\n\n" +
				"Possible aggregate statuses: **up** (all backends up), **partial** (some up / some down), " +
				"**down** (all down), **unknown** (not yet probed or no backends configured).",
			"parameters": []any{
				map[string]any{
					"name":        "verbose",
					"in":          "query",
					"required":    false,
					"description": "Include per-backend statuses and last probe timestamp in the response.",
					"schema":      map[string]any{"type": "boolean", "default": false},
				},
				map[string]any{
					"name":        "mode",
					"in":          "query",
					"required":    false,
					"description": "Set to `strict` to return HTTP 500 when the aggregate status is not `up` (partial / down / unknown). The response body is unchanged.",
					"schema":      map[string]any{"type": "string", "enum": []any{"strict"}},
				},
				map[string]any{
					"name":        "model",
					"in":          "query",
					"required":    false,
					"description": "Filter to a single model name. Implies backend lookup (same as verbose=true).",
					"schema":      map[string]any{"type": "string"},
				},
			},
			"responses": map[string]any{
				"200": map[string]any{
					"description": "Health status (aggregate up, partial, or unknown in non-strict mode)",
					"content": map[string]any{
						"application/json": map[string]any{
							"schema": map[string]any{
								"oneOf": []any{simpleResponseSchema, verboseResponseSchema},
							},
						},
					},
				},
				"500": map[string]any{
					"description": "One or more backends are not up (strict mode only)",
					"content": map[string]any{
						"application/json": map[string]any{
							"schema": verboseResponseSchema,
						},
					},
				},
				"503": map[string]any{
					"description": "Health cache unavailable (Redis error)",
					"content": map[string]any{
						"application/json": map[string]any{
							"schema": errorResponseSchema,
						},
					},
				},
			},
		},
	}
}

func submitJobPathItem(serviceTypes []string, reg *service.Registry) map[string]any {
	// Collect models and operations per service type.
	modelsByType := map[string][]string{}
	allOps := []string{}
	for _, def := range reg.All() {
		if def.Model != "" {
			modelsByType[def.Type] = uniqueSorted(append(modelsByType[def.Type], def.Model))
		}
		for opName := range def.Operations {
			allOps = appendUniq(allOps, opName)
		}
	}
	sort.Strings(allOps)

	// Build a readable description of models per type.
	typeParts := make([]string, 0, len(serviceTypes))
	for _, t := range serviceTypes {
		if models := modelsByType[t]; len(models) > 0 {
			typeParts = append(typeParts, fmt.Sprintf("**%s**: %s", t, strings.Join(models, ", ")))
		}
	}

	schemaProps := map[string]any{
		"file": map[string]any{
			"type":        "string",
			"format":      "binary",
			"description": "File to process",
		},
		"model": map[string]any{
			"type":        "string",
			"description": "Inference model. Required when multiple models are configured for the service type.\n\n" + strings.Join(typeParts, "\n\n"),
		},
		"callback_url": map[string]any{
			"type":        "string",
			"format":      "uri",
			"description": "Webhook URL called when the job completes",
			"example":     "https://your-app.example.com/webhook",
		},
	}
	if len(allOps) > 0 {
		opDesc := "Operation to perform. Required when the model has multiple operations."
		schemaProps["operation"] = map[string]any{
			"type":        "string",
			"enum":        allOps,
			"description": opDesc,
		}
	}

	return map[string]any{
		"post": map[string]any{
			"tags":        []string{"Jobs"},
			"summary":     "Submit an async inference job",
			"operationId": "submitJob",
			"parameters": []any{
				map[string]any{
					"name": "service_type", "in": "path", "required": true,
					"schema": map[string]any{"type": "string", "enum": serviceTypes},
					"example": serviceTypes[0],
				},
			},
			"requestBody": map[string]any{
				"required": true,
				"content": map[string]any{
					"multipart/form-data": map[string]any{
						"schema": map[string]any{
							"type":       "object",
							"required":   []string{"file"},
							"properties": schemaProps,
						},
					},
				},
			},
			"responses": map[string]any{
				"202": map[string]any{
					"description": "Job accepted",
					"content": map[string]any{
						"application/json": map[string]any{
							"schema": map[string]any{"$ref": "#/components/schemas/JobSubmitResponse"},
						},
					},
				},
				"400": map[string]any{"$ref": "#/components/responses/BadRequest"},
				"404": map[string]any{"$ref": "#/components/responses/NotFound"},
				"500": map[string]any{"$ref": "#/components/responses/InternalError"},
			},
		},
	}
}

// jobByIDPathItem returns the combined GET + DELETE path item for /jobs/{service_type}/{id}.
func jobByIDPathItem(serviceTypes []string) map[string]any {
	pathParams := []any{
		map[string]any{
			"name": "service_type", "in": "path", "required": true,
			"schema": map[string]any{"type": "string", "enum": serviceTypes},
		},
		map[string]any{
			"name": "id", "in": "path", "required": true,
			"schema":  map[string]any{"type": "string", "format": "uuid"},
			"example": "550e8400-e29b-41d4-a716-446655440000",
		},
	}
	return map[string]any{
		"get": map[string]any{
			"tags":        []string{"Jobs"},
			"summary":     "Get job status and result",
			"operationId": "getJob",
			"description": "Returns the current status of a job. When `completed`, the result is inlined.\n\n**The result file is deleted after this call** — subsequent calls return 404.",
			"parameters":  pathParams,
			"responses": map[string]any{
				"200": map[string]any{
					"description": "Job found",
					"content": map[string]any{
						"application/json": map[string]any{
							"schema": map[string]any{"$ref": "#/components/schemas/JobStatusResponse"},
						},
					},
				},
				"404": map[string]any{"$ref": "#/components/responses/NotFound"},
			},
		},
		"delete": map[string]any{
			"tags":        []string{"Jobs"},
			"summary":     "Cancel a job",
			"operationId": "cancelJob",
			"description": "Cancels a pending job and deletes its input file from S3. Returns 409 if the job is not in `pending` state: once the relay has started inference (`processing`, `completed`, or `failed`), cancellation is not allowed.\n\nApplies the same consumer ownership check as GET: if `consumer_header` is configured and present, only the owning consumer can cancel.",
			"parameters":  pathParams,
			"responses": map[string]any{
				"204": map[string]any{"description": "Job cancelled"},
				"404": map[string]any{"$ref": "#/components/responses/NotFound"},
				"409": map[string]any{
					"description": "Job is not cancellable in its current state",
					"content": map[string]any{
						"application/json": map[string]any{"schema": map[string]any{"$ref": "#/components/schemas/Error"}},
					},
				},
			},
		},
	}
}

func purgeJobsPathItem() map[string]any {
	return map[string]any{
		"post": map[string]any{
			"tags":        []string{"Jobs"},
			"summary":     "Admin: purge stale pending jobs",
			"operationId": "purgeJobs",
			"description": "Deletes pending jobs older than `older_than`. Also cleans up their S3 input files. Restricted to the `/-/` admin namespace — protect with upstream auth.\n\nIf `truncated=true` in the response, there are more matching jobs — call again until `truncated=false` to fully drain the queue.\n\n**Example:** `POST /-/jobs/purge?older_than=2h&limit=200`",
			"parameters": []any{
				map[string]any{
					"name":        "older_than",
					"in":          "query",
					"required":    true,
					"description": "Minimum age of jobs to purge, as a Go duration string (e.g. `2h`, `30m`)",
					"schema":      map[string]any{"type": "string", "example": "2h"},
				},
				map[string]any{
					"name":        "limit",
					"in":          "query",
					"required":    false,
					"description": "Maximum number of jobs to delete per call (default 500). Call repeatedly until `truncated=false` to fully drain.",
					"schema":      map[string]any{"type": "integer", "default": 500, "minimum": 1},
				},
			},
			"responses": map[string]any{
				"200": map[string]any{
					"description": "Purge result",
					"content": map[string]any{
						"application/json": map[string]any{
							"schema": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"older_than": map[string]any{"type": "string", "example": "2h"},
									"found":      map[string]any{"type": "integer", "description": "Jobs matched (capped at limit)"},
									"purged":     map[string]any{"type": "integer", "description": "Jobs successfully deleted"},
									"truncated":  map[string]any{"type": "boolean", "description": "True when more matching jobs exist beyond the limit"},
								},
							},
						},
					},
				},
				"400": map[string]any{"$ref": "#/components/responses/BadRequest"},
			},
		},
	}
}

func listModelsPathItem() map[string]any {
	return map[string]any{
		"get": map[string]any{
			"tags":    []string{"Inference"},
			"summary": "List available models",
			"description": "Without query params, returns all configured models in OpenAI-compatible format.\n\n" +
				"With `?model=<name>`, proxies to the underlying model backend to retrieve its native information " +
				"(context size, capabilities, etc.).",
			"operationId": "listModels",
			"parameters": []any{
				map[string]any{
					"name":        "model",
					"in":          "query",
					"required":    false,
					"description": "When provided, proxies the request to the underlying model backend and returns its native model information.",
					"schema":      map[string]any{"type": "string"},
				},
			},
			"responses": map[string]any{
				"200": map[string]any{
					"description": "Model list or native model information",
					"content": map[string]any{
						"application/json": map[string]any{
							"schema": map[string]any{"$ref": "#/components/schemas/ModelList"},
						},
					},
				},
				"404": map[string]any{"$ref": "#/components/responses/NotFound"},
				"502": map[string]any{"$ref": "#/components/responses/BadGateway"},
			},
		},
	}
}

// syncPathItems generates one path item per unique URL path found across all
// service definitions. Pattern paths (containing {model}) get a path parameter.
func syncPathItems(reg *service.Registry) map[string]any {
	type entry struct {
		serviceType string
		models      []string
		opNames     []string
		exts        []string
		isPattern   bool
	}

	byPath := map[string]*entry{}

	for _, def := range reg.All() {
		if def.InferenceURL == "" {
			continue
		}
		for opName, paths := range def.Operations {
			for _, p := range paths {
				if p == "" {
					continue
				}
				e := byPath[p]
				if e == nil {
					e = &entry{
						serviceType: def.Type,
						isPattern:   strings.Contains(p, "{model}"),
					}
					byPath[p] = e
				}
				e.models = appendUniq(e.models, def.Model)
				e.opNames = appendUniq(e.opNames, opName)
				for ext := range def.AcceptedExts {
					e.exts = appendUniq(e.exts, ext)
				}
			}
		}
	}

	items := map[string]any{}
	for p, e := range byPath {
		sort.Strings(e.models)
		sort.Strings(e.opNames)
		sort.Strings(e.exts)

		// Request body schema
		schemaProps := map[string]any{}
		required := []string{}

		if !e.isPattern {
			modelField := map[string]any{"type": "string"}
			if len(e.models) > 0 {
				modelField["enum"] = e.models
				modelField["example"] = e.models[0]
			}
			schemaProps["model"] = modelField
			if len(e.models) > 1 {
				required = append(required, "model")
			}
		}

		if len(e.opNames) > 1 {
			schemaProps["operation"] = map[string]any{
				"type":        "string",
				"enum":        e.opNames,
				"description": "Operation to perform. Required when the model supports multiple operations.",
			}
		}

		fileField := map[string]any{"type": "string", "format": "binary"}
		if len(e.exts) > 0 {
			fileField["description"] = "Accepted formats: " + strings.Join(e.exts, ", ")
		}
		schemaProps["file"] = fileField
		required = append(required, "file")

		// Summary from operation names
		summary := strings.Join(e.opNames, " / ")
		if summary != "" {
			summary = strings.ToUpper(summary[:1]) + summary[1:] + " (sync)"
		} else {
			summary = "Inference (sync)"
		}

		desc := "Direct proxy to the inference backend (`inference_url`)."

		op := map[string]any{
			"tags":        []string{e.serviceType},
			"summary":     summary,
			"description": desc,
			"requestBody": map[string]any{
				"required": true,
				"content": map[string]any{
					"multipart/form-data": map[string]any{
						"schema": map[string]any{
							"type":       "object",
							"required":   required,
							"properties": schemaProps,
						},
					},
				},
			},
			"responses": map[string]any{
				"200": map[string]any{
					"description": "Inference result",
					"content": map[string]any{
						"application/json": map[string]any{
							"schema": map[string]any{"type": "object"},
						},
					},
				},
				"400": map[string]any{"$ref": "#/components/responses/BadRequest"},
				"422": map[string]any{"$ref": "#/components/responses/UnprocessableEntity"},
				"504": map[string]any{"$ref": "#/components/responses/GatewayTimeout"},
				"502": map[string]any{"$ref": "#/components/responses/BadGateway"},
			},
		}

		if e.isPattern {
			op["parameters"] = []any{
				map[string]any{
					"name": "model", "in": "path", "required": true,
					"schema": map[string]any{"type": "string", "enum": e.models},
				},
			}
		}

		items[p] = map[string]any{"post": op}
	}

	return items
}

// ── Shared components ──────────────────────────────────────────────────────────

func specComponents() map[string]any {
	return map[string]any{
		"securitySchemes": map[string]any{
			"ApiKeyAuth": map[string]any{
				"type": "apiKey",
				"in":   "header",
				"name": "apikey",
			},
		},
		"schemas": map[string]any{
			"JobSubmitResponse": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"job_id":       map[string]any{"type": "string", "format": "uuid", "example": "550e8400-e29b-41d4-a716-446655440000"},
					"service_type": map[string]any{"type": "string", "example": "audio"},
					"model":        map[string]any{"type": "string", "example": "whisper-large-v3"},
					"status":       map[string]any{"type": "string", "example": "pending"},
				},
			},
			"JobStatusResponse": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"job_id":       map[string]any{"type": "string", "format": "uuid"},
					"service_type": map[string]any{"type": "string"},
					"model":        map[string]any{"type": "string"},
					"status":       map[string]any{"$ref": "#/components/schemas/JobStatus"},
					"queue_position": map[string]any{
						"type":        "integer",
						"description": "1-indexed position in the model queue. Present only when status is `pending`.",
					},
					"result":     map[string]any{"type": "object", "description": "Inline result payload — present only when status is `completed`"},
					"error":      map[string]any{"type": "string", "description": "Error message — present only when status is `failed`"},
					"created_at": map[string]any{"type": "string", "format": "date-time"},
					"updated_at": map[string]any{"type": "string", "format": "date-time"},
				},
			},
			"JobStatus": map[string]any{
				"type":    "string",
				"enum":    []string{"pending", "processing", "completed", "failed"},
				"example": "completed",
			},
			"ModelList": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"object": map[string]any{"type": "string", "example": "list"},
					"data": map[string]any{
						"type":  "array",
						"items": map[string]any{"$ref": "#/components/schemas/Model"},
					},
				},
			},
			"Model": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":       map[string]any{"type": "string", "example": "whisper-large-v3"},
					"object":   map[string]any{"type": "string", "example": "model"},
					"owned_by": map[string]any{"type": "string", "example": "gatewai"},
				},
			},
			"Error": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"error": map[string]any{"type": "string", "example": "field 'file' is required"},
				},
			},
		},
		"responses": map[string]any{
			"BadRequest": map[string]any{
				"description": "Invalid request",
				"content": map[string]any{
					"application/json": map[string]any{"schema": map[string]any{"$ref": "#/components/schemas/Error"}},
				},
			},
			"NotFound": map[string]any{
				"description": "Resource not found",
				"content": map[string]any{
					"application/json": map[string]any{"schema": map[string]any{"$ref": "#/components/schemas/Error"}},
				},
			},
			"UnprocessableEntity": map[string]any{
				"description": "Inference failed",
				"content": map[string]any{
					"application/json": map[string]any{"schema": map[string]any{"$ref": "#/components/schemas/Error"}},
				},
			},
			"InternalError": map[string]any{
				"description": "Internal server error",
				"content": map[string]any{
					"application/json": map[string]any{"schema": map[string]any{"$ref": "#/components/schemas/Error"}},
				},
			},
			"GatewayTimeout": map[string]any{
				"description": "Timed out waiting for inference result",
				"content": map[string]any{
					"application/json": map[string]any{"schema": map[string]any{"$ref": "#/components/schemas/Error"}},
				},
			},
			"BadGateway": map[string]any{
				"description": "Upstream inference backend error",
				"content": map[string]any{
					"application/json": map[string]any{"schema": map[string]any{"$ref": "#/components/schemas/Error"}},
				},
			},
		},
	}
}

// ── Helpers ────────────────────────────────────────────────────────────────────

func appendUniq(slice []string, s string) []string {
	for _, v := range slice {
		if v == s {
			return slice
		}
	}
	return append(slice, s)
}

func uniqueSorted(s []string) []string {
	seen := map[string]struct{}{}
	out := s[:0]
	for _, v := range s {
		if _, ok := seen[v]; !ok {
			seen[v] = struct{}{}
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}
