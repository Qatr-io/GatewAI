package guardrails

import (
	"context"
	"log/slog"
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

// MaxScore returns the highest confidence score among findings (0 if none). Used
// to record how confidently an async detector fired without exposing the matched
// text.
func MaxScore(findings []Finding) float64 {
	max := 0.0
	for _, f := range findings {
		if f.Score > max {
			max = f.Score
		}
	}
	return max
}

// FlaggedSample is the correlated metadata of a guardrail flag. It carries no
// prompt text itself — EmitFlaggedSample redacts the prompt and attaches it.
type FlaggedSample struct {
	Stage       string   // "input" | "output" | "async"
	Detector    string   // detector name that fired
	Categories  []string // categories it reported
	Score       float64  // max confidence of the finding, in [0,1]
	ServiceType string
	Model       string
	Consumer    string
}

// EmitFlaggedSample writes a correlated, PII-redacted record of a guardrail flag
// so shadow-mode (async) detections can be REVIEWED, not merely counted, to
// calibrate before flipping a detector to enforce.
//
// Unlike MarkSpanFlagged (which tags the live request span — useless for an async
// detector that fires after the request span has ended), this stands alone: it
// stamps the trace_id from ctx so a reviewer can still correlate the sample back
// to its trace, and it carries a copy of the prompt with PII stripped via
// redactGroups (never the raw text). It emits a structured slog record keyed by
// event="guardrail.flagged_sample"; where slog is bridged to OTLP logs the record
// lands in the operator's governed sink (Loki/Langfuse), whose retention enforces
// the sample's TTL. Gated by config — call only when sampling is enabled.
//
// redactGroups must be non-empty; an empty set would leak raw PII into the record,
// so callers resolve a safe default (pii+secrets) before calling.
func EmitFlaggedSample(ctx context.Context, s FlaggedSample, texts []string, redactGroups []string) {
	redacted, _ := RedactText(strings.Join(texts, "\n"), redactGroups)
	traceID := ""
	if sc := trace.SpanContextFromContext(ctx); sc.HasTraceID() {
		traceID = sc.TraceID().String()
	}
	slog.WarnContext(ctx, "guardrail flagged sample",
		"event", "guardrail.flagged_sample",
		"trace_id", traceID,
		"guardrail.stage", s.Stage,
		"guardrail.detector", s.Detector,
		"guardrail.categories", strings.Join(s.Categories, ","),
		"guardrail.score", s.Score,
		"service_type", s.ServiceType,
		"model", s.Model,
		"consumer", s.Consumer,
		"redacted_prompt", redacted,
	)
}
