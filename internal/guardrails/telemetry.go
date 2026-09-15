package guardrails

import (
	"context"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// MarkSpanFlagged tags the current trace span to record that a guardrail acted on
// the request, so traces can be filtered to flagged ones (e.g. in Tempo/Langfuse)
// and correlated with the prompt already on the span.
//
// It sets:
//   - guardrail.flagged   = true (the filterable boolean)
//   - guardrail.stage     = "input" | "output"
//   - guardrail.action    = "flag" | "block" | "redact"
//   - guardrail.detectors = comma-joined names of the checks/detectors that fired
//
// Safe and cheap: when tracing is disabled the context carries a no-op span, so
// this is a no-op. Only effective for SYNC/inline flag sites — an async (shadow)
// detector runs after the request span has ended, so tagging there would be lost;
// use a correlated record for that path instead.
func MarkSpanFlagged(ctx context.Context, stage, action string, detectors []string) {
	span := trace.SpanFromContext(ctx)
	if !span.IsRecording() {
		return
	}
	span.SetAttributes(
		attribute.Bool("guardrail.flagged", true),
		attribute.String("guardrail.stage", stage),
		attribute.String("guardrail.action", action),
		attribute.String("guardrail.detectors", strings.Join(detectors, ",")),
	)
}
