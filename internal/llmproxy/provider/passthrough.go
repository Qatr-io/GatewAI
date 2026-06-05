package provider

import (
	"bytes"
	"context"
	"fmt"
	"net/http"

	"gatewai/gateway/internal/service"
)

// passthroughProvider forwards requests verbatim to inference_url with
// inference_headers injected. Equivalent to the existing proxyToInference path.
type passthroughProvider struct{}

func (p *passthroughProvider) Name() string { return "passthrough" }

func (p *passthroughProvider) BuildRequest(ctx context.Context, def *service.Def, body []byte, urlPath string, baseURL string) (*http.Request, error) {
	target := baseURL + urlPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("passthrough: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range def.InferenceHeaders {
		req.Header.Set(k, v)
	}
	return req, nil
}

func (p *passthroughProvider) TranslateResponse(_ context.Context, status int, _ http.Header, body []byte) (int, []byte, *Usage, error) {
	// Passthrough targets OpenAI-compatible backends (vLLM, etc.) — parse usage
	// so token metrics are available to callers even without response translation.
	return status, body, parseOpenAIUsage(body), nil
}
