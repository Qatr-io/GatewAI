package adapter

import (
	"context"
	"io"
	"net/http"
	"strings"

	"kevent/relay/internal/config"
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
//
// We deliberately do NOT set http.Client.Timeout here: that hard-caps every
// request to the same value, which forced operators to set a single global
// timeout suitable for the slowest possible operation. Instead, each Call
// derives a per-request context.WithTimeout from cfg.Inference.TimeoutFor
// (multipart.go), so an operator can run a fast service (whisper, ~120s)
// and a slow one (pyannote diarization on 2h audio, ~3600s) behind the same
// relay binary with different per-operation overrides.
func New(cfg *config.Config) (Adapter, error) {
	return &multipartAdapter{
		inf:    cfg.Inference,
		client: &http.Client{},
	}, nil
}

// endpointURL constructs the full inference URL from the base and the per-event path.
func endpointURL(inf config.InferenceConfig, input CallInput) string {
	return strings.TrimRight(inf.BaseURL, "/") + input.InferenceURL
}
