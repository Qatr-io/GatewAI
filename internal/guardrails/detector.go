package guardrails

import "context"

// Finding is a single guardrail detection. It is detector-agnostic: the regex
// Checker and (future) model-backed detectors both emit Findings, so the
// pipeline can compose them under one set of block/redact/flag semantics.
type Finding struct {
	Category string  // matched category, e.g. "email", "aws_access_key", "injection"
	Detector string  // which detector produced it, e.g. "regex"
	Score    float64 // confidence in [0,1]; deterministic regex matches are 1.0
}

// Detector scans and optionally redacts text for guardrail violations. The regex
// Checker satisfies this via RegexDetector; model-backed detectors implement it
// by calling an inference endpoint. Implementations must be safe for concurrent
// use. The context carries deadlines so a slow detector can be bounded and
// failed open by the caller.
type Detector interface {
	// Name identifies the detector in metrics and logs (e.g. "regex").
	Name() string
	// Scan reports findings across the given already-extracted text fragments
	// (detection only — no mutation).
	Scan(ctx context.Context, texts []string) ([]Finding, error)
	// Redact rewrites an OpenAI-compatible message body in place, returning the
	// rewritten body (still valid JSON) and the findings that were redacted.
	// Detectors that cannot produce spans return the body unchanged alongside
	// their findings.
	Redact(ctx context.Context, body []byte) ([]byte, []Finding, error)
}

// RegexDetector adapts the pattern-based Checker to the Detector interface,
// scoped to a fixed set of enabled check groups. It changes no behavior: Scan
// and Redact delegate to the Checker and map its category names to Findings.
type RegexDetector struct {
	checker *Checker
	groups  []string
}

// NewRegexDetector returns a RegexDetector limited to the given check groups
// (e.g. CheckPII, CheckSecrets). An empty group list means all groups.
func NewRegexDetector(groups ...string) *RegexDetector {
	return &RegexDetector{checker: New(), groups: groups}
}

// Name implements Detector.
func (d *RegexDetector) Name() string { return "regex" }

// Scan implements Detector by matching the enabled regex groups against texts.
func (d *RegexDetector) Scan(_ context.Context, texts []string) ([]Finding, error) {
	return categoriesToFindings(d.checker.ScanStrings(texts, d.groups), d.Name()), nil
}

// Redact implements Detector by delegating to Checker.Redact.
func (d *RegexDetector) Redact(_ context.Context, body []byte) ([]byte, []Finding, error) {
	out, cats := d.checker.Redact(body, d.groups)
	return out, categoriesToFindings(cats, d.Name()), nil
}

// categoriesToFindings maps category names from the Checker into Findings,
// preserving order.
func categoriesToFindings(cats []string, detector string) []Finding {
	if len(cats) == 0 {
		return nil
	}
	out := make([]Finding, 0, len(cats))
	for _, c := range cats {
		out = append(out, Finding{Category: c, Detector: detector, Score: 1.0})
	}
	return out
}

// Categories returns just the category names from a Finding slice, preserving
// order. Helper for callers still working in terms of category-name lists.
func Categories(findings []Finding) []string {
	if len(findings) == 0 {
		return nil
	}
	out := make([]string, 0, len(findings))
	for _, f := range findings {
		out = append(out, f.Category)
	}
	return out
}

// MessageTexts extracts the scannable text fragments from an OpenAI-compatible
// request body (message content). Exported so detectors can share extraction.
func MessageTexts(body []byte) []string { return extractMessageTexts(body) }

// ResponseTexts extracts the scannable text fragments from an OpenAI-compatible
// response body (choices[*].message.content).
func ResponseTexts(body []byte) []string { return extractResponseTexts(body) }
