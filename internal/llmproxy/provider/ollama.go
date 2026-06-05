package provider

import (
	"bytes"
	"context"
	"fmt"
	"net/http"

	"gatewai/gateway/internal/service"
)

// ollamaProvider proxies to an Ollama instance using its OpenAI-compatible API.
// No request/response translation is needed.
type ollamaProvider struct{}

func (p *ollamaProvider) Name() string { return "ollama" }

func (p *ollamaProvider) BuildRequest(ctx context.Context, def *service.Def, body []byte, urlPath string, baseURL string) (*http.Request, error) {
	if baseURL == "" {
		baseURL = "http://localhost:11434"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+urlPath, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("ollama: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range def.InferenceHeaders {
		req.Header.Set(k, v)
	}
	return req, nil
}

func (p *ollamaProvider) TranslateResponse(_ context.Context, status int, _ http.Header, body []byte) (int, []byte, *Usage, error) {
	return status, body, parseOpenAIUsage(body), nil
}
