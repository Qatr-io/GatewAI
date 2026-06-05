package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"gatewai/gateway/internal/service"
)

type anthropicProvider struct{}

func (p *anthropicProvider) Name() string { return "anthropic" }

func (p *anthropicProvider) BuildRequest(ctx context.Context, def *service.Def, body []byte, urlPath string, baseURL string) (*http.Request, error) {
	translated, err := openAIToAnthropic(body)
	if err != nil {
		return nil, fmt.Errorf("anthropic: translate request: %w", err)
	}

	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}
	// Always use /v1/messages regardless of the client path.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/messages", bytes.NewReader(translated))
	if err != nil {
		return nil, fmt.Errorf("anthropic: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	for k, v := range def.InferenceHeaders {
		req.Header.Set(k, v)
	}
	return req, nil
}

func (p *anthropicProvider) TranslateResponse(_ context.Context, status int, _ http.Header, body []byte) (int, []byte, *Usage, error) {
	if status != http.StatusOK {
		// Pass through error responses as-is; callers log them.
		return status, body, nil, nil
	}
	translated, usage, err := anthropicToOpenAI(body)
	if err != nil {
		return http.StatusInternalServerError, nil, nil, fmt.Errorf("anthropic: translate response: %w", err)
	}
	return status, translated, usage, nil
}

// openAIRequest is the subset of the OpenAI chat completions schema we care about.
type openAIRequest struct {
	Model         string          `json:"model"`
	Messages      []openAIMessage `json:"messages"`
	MaxTokens     *int            `json:"max_tokens,omitempty"`
	MaxCompTokens *int            `json:"max_completion_tokens,omitempty"`
	Temperature   *float64        `json:"temperature,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	Stop          any             `json:"stop,omitempty"`
	Stream        bool            `json:"stream,omitempty"`
	Tools         []any           `json:"tools,omitempty"`
	ToolChoice    any             `json:"tool_choice,omitempty"`
}

type openAIMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string or []ContentPart
}

func openAIToAnthropic(body []byte) ([]byte, error) {
	var req openAIRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}

	type anthropicMessage struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	type anthropicRequest struct {
		Model       string             `json:"model"`
		Messages    []anthropicMessage `json:"messages"`
		System      string             `json:"system,omitempty"`
		MaxTokens   int                `json:"max_tokens"`
		Temperature *float64           `json:"temperature,omitempty"`
		TopP        *float64           `json:"top_p,omitempty"`
		StopSeqs    []string           `json:"stop_sequences,omitempty"`
		Stream      bool               `json:"stream,omitempty"`
	}

	var system string
	var msgs []anthropicMessage
	for _, m := range req.Messages {
		content := contentToString(m.Content)
		if m.Role == "system" {
			if system != "" {
				system += "\n"
			}
			system += content
			continue
		}
		role := m.Role
		if role != "user" && role != "assistant" {
			role = "user"
		}
		msgs = append(msgs, anthropicMessage{Role: role, Content: content})
	}

	maxTok := 4096
	if req.MaxTokens != nil {
		maxTok = *req.MaxTokens
	} else if req.MaxCompTokens != nil {
		maxTok = *req.MaxCompTokens
	}

	var stopSeqs []string
	switch v := req.Stop.(type) {
	case string:
		if v != "" {
			stopSeqs = []string{v}
		}
	case []any:
		for _, s := range v {
			if str, ok := s.(string); ok {
				stopSeqs = append(stopSeqs, str)
			}
		}
	}

	out := anthropicRequest{
		Model:       req.Model,
		Messages:    msgs,
		System:      system,
		MaxTokens:   maxTok,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		StopSeqs:    stopSeqs,
		Stream:      req.Stream,
	}
	return json.Marshal(out)
}

func contentToString(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, part := range v {
			if m, ok := part.(map[string]any); ok {
				if t, ok := m["text"].(string); ok {
					b.WriteString(t)
				}
			}
		}
		return b.String()
	default:
		return ""
	}
}

type anthropicResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Type    string `json:"type"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

func anthropicToOpenAI(body []byte) ([]byte, *Usage, error) {
	var resp anthropicResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, nil, err
	}

	var text string
	for _, block := range resp.Content {
		if block.Type == "text" {
			text += block.Text
		}
	}

	finishReason := anthropicFinishReason(resp.StopReason)

	type choice struct {
		Index        int    `json:"index"`
		Message      any    `json:"message"`
		FinishReason string `json:"finish_reason"`
	}
	type usageOut struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	}
	type openAIResp struct {
		ID      string   `json:"id"`
		Object  string   `json:"object"`
		Model   string   `json:"model"`
		Choices []choice `json:"choices"`
		Usage   usageOut `json:"usage"`
	}

	out := openAIResp{
		ID:     resp.ID,
		Object: "chat.completion",
		Model:  resp.Model,
		Choices: []choice{{
			Index:        0,
			Message:      map[string]any{"role": "assistant", "content": text},
			FinishReason: finishReason,
		}},
		Usage: usageOut{
			PromptTokens:     resp.Usage.InputTokens,
			CompletionTokens: resp.Usage.OutputTokens,
			TotalTokens:      resp.Usage.InputTokens + resp.Usage.OutputTokens,
		},
	}

	translated, err := json.Marshal(out)
	if err != nil {
		return nil, nil, err
	}
	usage := &Usage{
		PromptTokens:     resp.Usage.InputTokens,
		CompletionTokens: resp.Usage.OutputTokens,
	}
	return translated, usage, nil
}

func anthropicFinishReason(stopReason string) string {
	switch stopReason {
	case "end_turn":
		return "stop"
	case "max_tokens":
		return "length"
	case "stop_sequence":
		return "stop"
	case "tool_use":
		return "tool_calls"
	default:
		return stopReason
	}
}
