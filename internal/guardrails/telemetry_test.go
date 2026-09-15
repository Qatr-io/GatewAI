package guardrails

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestMarkSpanFlagged_SetsAttributes(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	ctx, span := tp.Tracer("test").Start(context.Background(), "req")

	MarkSpanFlagged(ctx, "input", "flag", []string{"pii", "injection"})
	span.End()

	ended := rec.Ended()
	if len(ended) != 1 {
		t.Fatalf("want 1 ended span, got %d", len(ended))
	}
	got := map[string]string{}
	flagged := false
	for _, a := range ended[0].Attributes() {
		switch string(a.Key) {
		case "guardrail.flagged":
			flagged = a.Value.AsBool()
		case "guardrail.stage", "guardrail.action", "guardrail.detectors":
			got[string(a.Key)] = a.Value.AsString()
		}
	}
	if !flagged {
		t.Error("guardrail.flagged should be true")
	}
	if got["guardrail.stage"] != "input" || got["guardrail.action"] != "flag" || got["guardrail.detectors"] != "pii,injection" {
		t.Errorf("unexpected attributes: %+v", got)
	}
}

// A context with no tracer carries a non-recording (no-op) span; MarkSpanFlagged
// must be a safe no-op there (tracing disabled path).
func TestMarkSpanFlagged_NoopWhenNotRecording(t *testing.T) {
	MarkSpanFlagged(context.Background(), "output", "block", []string{"secrets"})
}

func TestMaxScore(t *testing.T) {
	if got := MaxScore(nil); got != 0 {
		t.Errorf("empty findings: want 0, got %v", got)
	}
	findings := []Finding{{Score: 0.3}, {Score: 0.91}, {Score: 0.5}}
	if got := MaxScore(findings); got != 0.91 {
		t.Errorf("want 0.91, got %v", got)
	}
}

// EmitFlaggedSample must redact PII from the prompt before it enters the record,
// and stamp the trace_id so the sample correlates back to its (ended) trace.
func TestEmitFlaggedSample_RedactsAndCorrelates(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	tp := sdktrace.NewTracerProvider()
	ctx, span := tp.Tracer("test").Start(context.Background(), "req")
	wantTrace := span.SpanContext().TraceID().String()
	span.End() // the async detector fires after the request span has ended

	EmitFlaggedSample(ctx, FlaggedSample{
		Stage: "input", Detector: "injection", Categories: []string{"injection"},
		Score: 0.97, ServiceType: "llm", Model: "gpt-4o", Consumer: "acme",
	}, []string{"contact me at alice@example.com please"}, []string{CheckPII})

	var rec map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
		t.Fatalf("record is not valid JSON: %v\n%s", err, buf.String())
	}
	if rec["event"] != "guardrail.flagged_sample" {
		t.Errorf("event = %v", rec["event"])
	}
	if rec["trace_id"] != wantTrace {
		t.Errorf("trace_id = %v, want %v", rec["trace_id"], wantTrace)
	}
	prompt, _ := rec["redacted_prompt"].(string)
	if strings.Contains(prompt, "alice@example.com") {
		t.Errorf("raw PII leaked into the sample: %q", prompt)
	}
	if !strings.Contains(prompt, "REDACTED") {
		t.Errorf("expected a redaction placeholder in %q", prompt)
	}
	if rec["consumer"] != "acme" || rec["model"] != "gpt-4o" || rec["guardrail.detector"] != "injection" {
		t.Errorf("unexpected metadata: %+v", rec)
	}
}
