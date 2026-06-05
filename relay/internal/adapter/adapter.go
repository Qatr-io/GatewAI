package adapter

import (
	"context"
	"io"
	"net/http"
	"strings"

	"gatewai/relay/internal/config"
)

// CallInput contains the data passed to the adapter for inference.
type CallInput struct {
	JobID        string
	Filename     string
	ContentType  string
	Size         int64             // -1 if unknown
	Body         io.Reader         // stream from S3; caller closes
	Model        string            // model name from InputEvent (e.g. "whisper-large-v3")
	InferenceURL string            // OpenAI path from InputEvent (e.g. "/v1/audio/transcriptions")
	Params       map[string]string // extra form fields forwarded from the client request
}

// Adapter sends an inference request to the local model and returns the raw JSON response.
type Adapter interface {
	Call(ctx context.Context, input CallInput) ([]byte, error)
}

// New returns the single multipart adapter backed by cfg.Inference.
func New(cfg *config.Config) (Adapter, error) {
	return &multipartAdapter{
		inf:    cfg.Inference,
		client: &http.Client{Timeout: cfg.Inference.TimeoutDuration()},
	}, nil
}

// endpointURL constructs the full inference URL from the base and the per-event path.
func endpointURL(inf config.InferenceConfig, input CallInput) string {
	return strings.TrimRight(inf.BaseURL, "/") + input.InferenceURL
}
