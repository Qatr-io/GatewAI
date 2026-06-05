package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"gatewai/gateway/internal/service"
)

type openAIProvider struct{}

func (p *openAIProvider) Name() string { return "openai" }

func (p *openAIProvider) BuildRequest(ctx context.Context, def *service.Def, body []byte, urlPath string, baseURL string) (*http.Request, error) {
	if baseURL == "" {
		baseURL = "https://api.openai.com"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+urlPath, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("openai: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range def.InferenceHeaders {
		req.Header.Set(k, v)
	}
	return req, nil
}

func (p *openAIProvider) TranslateResponse(_ context.Context, status int, _ http.Header, body []byte) (int, []byte, *Usage, error) {
	usage := parseOpenAIUsage(body)
	return status, body, usage, nil
}

func parseOpenAIUsage(body []byte) *Usage {
	var resp struct {
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil
	}
	return &Usage{
		PromptTokens:     resp.Usage.PromptTokens,
		CompletionTokens: resp.Usage.CompletionTokens,
	}
}
