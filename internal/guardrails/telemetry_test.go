package guardrails

import (
	"context"
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
