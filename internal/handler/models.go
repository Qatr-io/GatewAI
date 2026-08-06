package handler

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"sort"
	"time"

	"gatewai/gateway/internal/service"
)

var modelsProxyClient = &http.Client{Timeout: 30 * time.Second}

// modelCapabilities describes what a model supports.
type modelCapabilities struct {
	SupportsAsync     bool     `json:"supports_async"`
	SupportsSync      bool     `json:"supports_sync"`
	SupportsStreaming bool     `json:"supports_streaming"`
	AcceptedFormats   []string `json:"accepted_formats,omitempty"`
	MaxFileSizeMB     int64    `json:"max_file_size_mb,omitempty"`
	Operations        []string `json:"operations,omitempty"`
	Deprecated        bool     `json:"deprecated,omitempty"`
}

// modelObject mirrors the OpenAI model object returned by GET /v1/models,
// extended with GatewAI-specific capability metadata.
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

// ListModels handles GET /v1/models.
// Without query params, returns all configured models in OpenAI-compatible format.
// With ?model=<name>, proxies to the underlying model backend to retrieve its native info.
func ListModels(registry *service.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if modelName := r.URL.Query().Get("model"); modelName != "" {
			proxyModelsToBackend(w, r, registry, modelName)
			return
		}

		defs := registry.Models()

		data := make([]modelObject, 0, len(defs))
		for _, d := range defs {
			data = append(data, modelObject{
				ID:          d.Model,
				Object:      "model",
				OwnedBy:     "gatewai",
				ServiceType: d.Type,
				Provider:    d.Provider,
				Capabilities: modelCapabilities{
					SupportsAsync:     d.SupportsAsync,
					SupportsSync:      len(d.Backends) > 0 || d.InferenceURL != "",
					SupportsStreaming: d.Provider != "",
					AcceptedFormats:   sortedExts(d.AcceptedExts),
					MaxFileSizeMB:     d.MaxFileSizeMB,
					Operations:        sortedKeys(d.Operations),
					Deprecated:        d.Deprecated,
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

func proxyModelsToBackend(w http.ResponseWriter, r *http.Request, reg *service.Registry, modelName string) {
	var found *service.Def
	for _, d := range reg.Models() {
		if d.Model == modelName && d.InferenceURL != "" {
			found = d
			break
		}
	}
	if found == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"model not found or has no backend"}`))
		return
	}

	u, err := url.Parse(found.InferenceURL)
	if err != nil || u.Host == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"invalid backend URL"}`))
		return
	}
	backendURL := u.Scheme + "://" + u.Host + "/v1/models"

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, backendURL, nil)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"failed to build backend request"}`))
		return
	}
	for k, v := range found.InferenceHeaders {
		req.Header.Set(k, v)
	}

	resp, err := modelsProxyClient.Do(req)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":"backend unreachable"}`))
		return
	}
	defer resp.Body.Close()

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
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
