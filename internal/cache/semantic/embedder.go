package semantic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// NewHTTPEmbedder returns an EmbedderFunc that calls an OpenAI-compatible
// /v1/embeddings endpoint.
//
//	POST {serviceURL}/v1/embeddings
//	{"model": model, "input": text}
//
// httpClient may be nil (a default 10-second client is used).
func NewHTTPEmbedder(serviceURL, model string, httpClient *http.Client) EmbedderFunc {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	endpoint := strings.TrimRight(serviceURL, "/") + "/v1/embeddings"
	return func(ctx context.Context, text string) ([]float32, error) {
		reqBody, err := json.Marshal(map[string]any{
			"model": model,
			"input": text,
		})
		if err != nil {
			return nil, fmt.Errorf("embed marshal: %w", err)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(reqBody))
		if err != nil {
			return nil, fmt.Errorf("embed request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("embed HTTP: %w", err)
		}
		defer resp.Body.Close()

		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("embed read response: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("embed service returned %d: %s", resp.StatusCode, raw)
		}

		var result struct {
			Data []struct {
				Embedding []float32 `json:"embedding"`
			} `json:"data"`
		}
		if err := json.Unmarshal(raw, &result); err != nil {
			return nil, fmt.Errorf("embed parse response: %w", err)
		}
		if len(result.Data) == 0 || len(result.Data[0].Embedding) == 0 {
			return nil, fmt.Errorf("embed service returned empty embedding")
		}
		return result.Data[0].Embedding, nil
	}
}
