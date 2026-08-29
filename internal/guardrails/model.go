package guardrails

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// defaultModelTimeout bounds a model-detector call when the config leaves it
// unset. Kept tight because model detectors run on the sync LLM path.
const defaultModelTimeout = 120 * time.Millisecond

// OnError selects what a model detector does when its call fails (timeout,
// unreachable, bad response).
type OnError string

const (
	// FailOpen forwards the request unguarded on detector failure (default).
	FailOpen OnError = "fail_open"
	// FailClosed surfaces an error so the pipeline blocks on detector failure.
	FailClosed OnError = "fail_closed"
)

// ModelConfig configures a single model-backed detector. mode/action are
// pipeline-level concerns applied by the caller, not the detector — the detector
// only detects.
type ModelConfig struct {
	Name       string        // detector name (metrics/logs)
	Endpoint   string        // HTTP endpoint the guardrail server exposes
	Categories []string      // categories to request/keep; empty = all returned
	Threshold  float64       // minimum score to keep a finding
	Timeout    time.Duration // per-call timeout (default 120ms)
	OnError    OnError       // fail_open (default) | fail_closed
}

// modelRequest / modelResponse define the JSON contract with the guardrail
// server: send the extracted texts (+ optional category filter), receive scored
// findings.
type modelRequest struct {
	Texts      []string `json:"texts"`
	Categories []string `json:"categories,omitempty"`
}

type modelResponse struct {
	Findings []struct {
		Category string  `json:"category"`
		Score    float64 `json:"score"`
	} `json:"findings"`
}

// badResponseError marks a non-2xx status or unparseable body, so failures can
// be classified for metrics.
type badResponseError struct{ msg string }

func (e *badResponseError) Error() string { return e.msg }

// ModelDetector calls a self-hosted guardrail model over HTTP and maps its
// scored findings to the Detector interface. Safe for concurrent use.
type ModelDetector struct {
	cfg    ModelConfig
	client *http.Client
}

// NewModelDetector returns a ModelDetector, applying defaults for timeout and
// error mode.
func NewModelDetector(cfg ModelConfig) *ModelDetector {
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultModelTimeout
	}
	if cfg.OnError == "" {
		cfg.OnError = FailOpen
	}
	return &ModelDetector{cfg: cfg, client: &http.Client{}}
}

// Name implements Detector.
func (d *ModelDetector) Name() string { return d.cfg.Name }

// Scan implements Detector: it calls the model, records latency, and applies the
// fail-open/closed policy. On a failure with FailOpen it returns no findings and
// no error (forward unguarded); with FailClosed it returns the error so the
// caller can block.
func (d *ModelDetector) Scan(ctx context.Context, texts []string) ([]Finding, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, d.cfg.Timeout)
	defer cancel()

	start := time.Now()
	raw, err := d.call(ctx, texts)
	observer.ObserveModelLatency(d.Name(), time.Since(start).Seconds())

	if err != nil {
		observer.IncModelError(d.Name(), classifyErr(err))
		if d.cfg.OnError == FailClosed {
			return nil, err
		}
		return nil, nil // fail open
	}
	return d.filter(raw), nil
}

// Redact implements Detector. A classifier model produces no spans, so it cannot
// rewrite the body — it returns the body unchanged alongside its findings.
// Span-based redaction (NER models) is a later slice.
func (d *ModelDetector) Redact(ctx context.Context, body []byte) ([]byte, []Finding, error) {
	f, err := d.Scan(ctx, MessageTexts(body))
	return body, f, err
}

// call performs the HTTP request and decodes the response.
func (d *ModelDetector) call(ctx context.Context, texts []string) (*modelResponse, error) {
	payload, err := json.Marshal(modelRequest{Texts: texts, Categories: d.cfg.Categories})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.cfg.Endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := d.client.Do(req)
	if err != nil {
		return nil, err // network error or context deadline
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, &badResponseError{msg: fmt.Sprintf("guardrail model %q returned status %d", d.Name(), resp.StatusCode)}
	}
	var mr modelResponse
	if err := json.NewDecoder(resp.Body).Decode(&mr); err != nil {
		return nil, &badResponseError{msg: fmt.Sprintf("guardrail model %q: decode response: %v", d.Name(), err)}
	}
	return &mr, nil
}

// filter keeps findings at or above the threshold and (when Categories is set)
// within the requested categories, mapping them to detector-tagged Findings.
func (d *ModelDetector) filter(mr *modelResponse) []Finding {
	if mr == nil || len(mr.Findings) == 0 {
		return nil
	}
	allow := map[string]bool{}
	for _, c := range d.cfg.Categories {
		allow[c] = true
	}
	var out []Finding
	for _, f := range mr.Findings {
		if f.Score < d.cfg.Threshold {
			continue
		}
		if len(allow) > 0 && !allow[f.Category] {
			continue
		}
		out = append(out, Finding{Category: f.Category, Detector: d.Name(), Score: f.Score})
	}
	return out
}

// classifyErr maps a call error to a metric reason.
func classifyErr(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	var bad *badResponseError
	if errors.As(err, &bad) {
		return "bad_response"
	}
	return "unreachable"
}

// Run scans texts through all detectors concurrently and aggregates their
// findings. Detectors run in parallel, so total latency is the slowest detector,
// not the sum. If any detector returns an error (a FailClosed model that failed),
// the first such error is returned alongside whatever findings were collected, so
// the caller can block.
func Run(ctx context.Context, detectors []Detector, texts []string) ([]Finding, error) {
	if len(detectors) == 0 {
		return nil, nil
	}
	type result struct {
		findings []Finding
		err      error
	}
	ch := make(chan result, len(detectors))
	for _, det := range detectors {
		go func(det Detector) {
			f, err := det.Scan(ctx, texts)
			ch <- result{findings: f, err: err}
		}(det)
	}
	var all []Finding
	var firstErr error
	for range detectors {
		r := <-ch
		if r.err != nil && firstErr == nil {
			firstErr = r.err
		}
		all = append(all, r.findings...)
	}
	return all, firstErr
}
