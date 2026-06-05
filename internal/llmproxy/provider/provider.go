// Package provider implements pluggable LLM backend translators.
// Each provider adapts between the internal OpenAI-format request and
// the target API's wire format.
package provider

import (
	"context"
	"fmt"
	"net/http"

	"gatewai/gateway/internal/service"
)

// Usage holds token counts parsed from a provider response.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
}

// Provider adapts requests to a specific LLM backend.
type Provider interface {
	// Name returns the provider identifier ("openai", "anthropic", etc.).
	Name() string
	// BuildRequest constructs an outbound HTTP request from an OpenAI-format body.
	// baseURL is the selected backend URL; providers use it instead of def.InferenceURL.
	// An empty baseURL falls back to the provider's built-in default (e.g. https://api.openai.com).
	BuildRequest(ctx context.Context, def *service.Def, openAIBody []byte, urlPath string, baseURL string) (*http.Request, error)
	// TranslateResponse converts the provider's response back to OpenAI format.
	// Returns the normalised status, body, usage counts, and any error.
	TranslateResponse(ctx context.Context, status int, headers http.Header, body []byte) (int, []byte, *Usage, error)
}

// Registry maps provider names to their implementations.
type Registry struct {
	byName map[string]Provider
}

// NewRegistry returns a Registry pre-loaded with all built-in providers.
func NewRegistry() *Registry {
	r := &Registry{byName: make(map[string]Provider)}
	for _, p := range []Provider{
		&openAIProvider{},
		&anthropicProvider{},
		&ollamaProvider{},
		&passthroughProvider{},
	} {
		r.byName[p.Name()] = p
	}
	return r
}

// Get returns the Provider for the given name, or an error if unknown.
func (r *Registry) Get(name string) (Provider, error) {
	p, ok := r.byName[name]
	if !ok {
		return nil, fmt.Errorf("unknown provider %q", name)
	}
	return p, nil
}
