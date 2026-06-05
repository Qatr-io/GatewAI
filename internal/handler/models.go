package handler

import (
	"encoding/json"
	"net/http"
	"sort"

	"gatewai/gateway/internal/service"
)

// modelCapabilities describes what a model supports.
type modelCapabilities struct {
	SupportsAsync    bool     `json:"supports_async"`
	SupportsSync     bool     `json:"supports_sync"`
	SupportsStreaming bool     `json:"supports_streaming"`
	AcceptedFormats  []string `json:"accepted_formats,omitempty"`
	MaxFileSizeMB    int64    `json:"max_file_size_mb,omitempty"`
	Operations       []string `json:"operations,omitempty"`
}

// modelObject mirrors the OpenAI model object returned by GET /v1/models,
// extended with kevent-specific capability metadata.
type modelObject struct {
	ID           string            `json:"id"`
	Object       string            `json:"object"`
	OwnedBy      string            `json:"owned_by"`
	ServiceType  string            `json:"service_type"`
	Provider     string            `json:"provider,omitempty"`
	Capabilities modelCapabilities `json:"capabilities"`
}

type modelsListResponse struct {
	Object string        `json:"object"`
	Data   []modelObject `json:"data"`
}

// ListModels handles GET /v1/models and returns all configured models
// in the OpenAI-compatible format, enriched with capability metadata.
func ListModels(registry *service.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		defs := registry.Models()

		data := make([]modelObject, 0, len(defs))
		for _, d := range defs {
			data = append(data, modelObject{
				ID:          d.Model,
				Object:      "model",
				OwnedBy:     "kevent",
				ServiceType: d.Type,
				Provider:    d.Provider,
				Capabilities: modelCapabilities{
					SupportsAsync:    d.SupportsAsync,
					SupportsSync:     len(d.Backends) > 0 || d.InferenceURL != "",
					SupportsStreaming: d.Provider != "",
					AcceptedFormats:  sortedExts(d.AcceptedExts),
					MaxFileSizeMB:    d.MaxFileSizeMB,
					Operations:       sortedKeys(d.Operations),
				},
			})
		}
		sort.Slice(data, func(i, j int) bool { return data[i].ID < data[j].ID })

		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(modelsListResponse{Object: "list", Data: data})
	}
}

func sortedExts(m map[string]struct{}) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedKeys(m map[string][]string) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
