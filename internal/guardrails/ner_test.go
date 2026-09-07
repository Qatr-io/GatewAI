package guardrails_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gatewai/gateway/internal/guardrails"
)

// nerServer masks a fixed secret in each input text and reports it as a finding,
// mimicking a span/NER redaction model.
func nerServer(t *testing.T, secret, placeholder string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Texts []string `json:"texts"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		var out struct {
			Findings []map[string]any `json:"findings"`
			Redacted []string         `json:"redacted_texts"`
		}
		for _, t := range req.Texts {
			masked := strings.ReplaceAll(t, secret, placeholder)
			out.Redacted = append(out.Redacted, masked)
			if masked != t {
				out.Findings = append(out.Findings, map[string]any{"category": "email", "score": 0.99, "text": secret})
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestModelDetector_NER_Redact(t *testing.T) {
	srv := nerServer(t, "bob@example.org", "[REDACTED_EMAIL]")
	d := guardrails.NewModelDetector(guardrails.ModelConfig{
		Name: "pii", Endpoint: srv.URL, Kind: guardrails.KindNER, Threshold: 0.5, Timeout: time.Second,
	})
	body := []byte(`{"messages":[{"role":"user","content":"my email is bob@example.org thanks"}]}`)

	out, findings, err := d.Redact(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "bob@example.org") {
		t.Errorf("email should have been redacted, got: %s", out)
	}
	if !strings.Contains(string(out), "[REDACTED_EMAIL]") {
		t.Errorf("expected placeholder in body, got: %s", out)
	}
	if len(findings) != 1 || findings[0].Category != "email" {
		t.Errorf("expected one email finding, got %+v", findings)
	}
}

func TestModelDetector_NER_Redact_NoMatch_BodyUnchanged(t *testing.T) {
	srv := nerServer(t, "bob@example.org", "[REDACTED_EMAIL]")
	d := guardrails.NewModelDetector(guardrails.ModelConfig{
		Name: "pii", Endpoint: srv.URL, Kind: guardrails.KindNER, Threshold: 0.5, Timeout: time.Second,
	})
	body := []byte(`{"messages":[{"role":"user","content":"nothing sensitive here"}]}`)
	out, findings, err := d.Redact(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(body) {
		t.Errorf("body should be unchanged when nothing matches:\n got: %s\nwant: %s", out, body)
	}
	if len(findings) != 0 {
		t.Errorf("expected no findings, got %+v", findings)
	}
}

func TestEvaluateRedact_Sequential(t *testing.T) {
	emailSrv := nerServer(t, "bob@example.org", "[REDACTED_EMAIL]")
	phoneSrv := nerServer(t, "0612345678", "[REDACTED_PHONE]")
	models := []guardrails.Enforcement{
		{Detector: guardrails.NewModelDetector(guardrails.ModelConfig{Name: "email", Endpoint: emailSrv.URL, Kind: guardrails.KindNER, Threshold: 0.5, Timeout: time.Second}), Mode: guardrails.ModeSync, Action: guardrails.ActionRedact},
		{Detector: guardrails.NewModelDetector(guardrails.ModelConfig{Name: "phone", Endpoint: phoneSrv.URL, Kind: guardrails.KindNER, Threshold: 0.5, Timeout: time.Second}), Mode: guardrails.ModeSync, Action: guardrails.ActionRedact},
	}
	body := []byte(`{"messages":[{"role":"user","content":"mail bob@example.org tel 0612345678"}]}`)

	out, results := guardrails.EvaluateRedact(context.Background(), models, body)
	if strings.Contains(string(out), "bob@example.org") || strings.Contains(string(out), "0612345678") {
		t.Errorf("both PII should be redacted, got: %s", out)
	}
	if len(results) != 2 {
		t.Errorf("expected two redact results, got %+v", results)
	}
}

func TestEvaluateSync_SkipsRedactModels(t *testing.T) {
	srv := nerServer(t, "x", "y")
	models := []guardrails.Enforcement{
		{Detector: guardrails.NewModelDetector(guardrails.ModelConfig{Name: "pii", Endpoint: srv.URL, Kind: guardrails.KindNER, Timeout: time.Second}), Mode: guardrails.ModeSync, Action: guardrails.ActionRedact},
	}
	// A redact model must not surface in the block/flag evaluation.
	if got := guardrails.EvaluateSync(context.Background(), models, []string{"x"}); got != nil {
		t.Errorf("EvaluateSync must skip redact-action models, got %+v", got)
	}
}
