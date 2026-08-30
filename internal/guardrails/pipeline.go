package guardrails

import "context"

// Mode and Action constants for model enforcement.
const (
	ModeSync  = "sync"  // inline: verdict arrives in time to block/redact
	ModeAsync = "async" // out-of-band shadow: observe only

	ActionBlock  = "block"
	ActionRedact = "redact"
	ActionFlag   = "flag"
)

// Enforcement pairs a detector with how the pipeline applies it: sync (inline,
// can act) or async (shadow, observe only), and the action taken on a finding.
type Enforcement struct {
	Detector Detector
	Mode     string // ModeSync | ModeAsync
	Action   string // ActionBlock | ActionRedact | ActionFlag
}

// ModelResult is one sync detector's outcome, for the caller to enforce.
type ModelResult struct {
	Name       string   // detector name
	Action     string   // block | flag (redact coerced to flag for classifiers)
	Categories []string // findings that fired (empty when only Err is set)
	Err        error    // non-nil = a fail_closed detector failed; caller should block
}

// Fired reports whether this result should cause the caller to act (a detector
// found something, or a fail_closed detector errored).
func (r ModelResult) Fired() bool { return r.Err != nil || len(r.Categories) > 0 }

// EvaluateSync runs every sync-mode enforcement in models concurrently against
// texts and returns the results that fired (found something or errored). Total
// latency is the slowest detector, not the sum. The caller inspects each
// result's Action to decide block vs flag. Async enforcements are ignored here —
// use FireAsync for those.
func EvaluateSync(ctx context.Context, models []Enforcement, texts []string) []ModelResult {
	// Only sync block/flag enforcements are evaluated here; redact models mutate
	// the body and are applied separately (see EvaluateRedact).
	sync := make([]Enforcement, 0, len(models))
	for _, m := range models {
		if m.Mode == ModeSync && m.Action != ActionRedact {
			sync = append(sync, m)
		}
	}
	if len(sync) == 0 || len(texts) == 0 {
		return nil
	}

	type item struct {
		idx int
		r   ModelResult
	}
	ch := make(chan item, len(sync))
	for i, m := range sync {
		go func(i int, m Enforcement) {
			findings, err := m.Detector.Scan(ctx, texts)
			r := ModelResult{Name: m.Detector.Name(), Action: m.Action}
			if err != nil {
				r.Err = err
			} else {
				r.Categories = Categories(findings)
			}
			ch <- item{i, r}
		}(i, m)
	}
	ordered := make([]ModelResult, len(sync))
	for range sync {
		it := <-ch
		ordered[it.idx] = it.r
	}
	var out []ModelResult
	for _, r := range ordered {
		if r.Fired() {
			out = append(out, r)
		}
	}
	return out
}

// RedactResult is one redact detector's outcome, for the caller to log/meter.
type RedactResult struct {
	Name       string
	Categories []string // categories redacted (empty when only Err is set)
	Err        error    // non-nil = a fail_closed NER detector failed
}

// EvaluateRedact applies every sync redact-action enforcement to body in
// sequence (each redacts the output of the previous), returning the possibly
// modified body and the results that fired (redacted something or errored).
// Sequential because each detector mutates the body.
func EvaluateRedact(ctx context.Context, models []Enforcement, body []byte) ([]byte, []RedactResult) {
	out := body
	var results []RedactResult
	for _, m := range models {
		if m.Mode != ModeSync || m.Action != ActionRedact {
			continue
		}
		cleaned, findings, err := m.Detector.Redact(ctx, out)
		if err != nil {
			results = append(results, RedactResult{Name: m.Detector.Name(), Err: err})
			continue
		}
		if len(findings) > 0 {
			out = cleaned
			results = append(results, RedactResult{Name: m.Detector.Name(), Categories: Categories(findings)})
		}
	}
	return out, results
}

// FireAsync runs every async-mode enforcement in the background against texts,
// calling observe(name, categories) for each detector that fires. Errors are
// swallowed (best-effort shadowing). The context MUST NOT be tied to the request
// lifetime, since the request returns before these complete.
func FireAsync(ctx context.Context, models []Enforcement, texts []string, observe func(name string, categories []string)) {
	if len(texts) == 0 {
		return
	}
	for _, m := range models {
		if m.Mode != ModeAsync {
			continue
		}
		go func(m Enforcement) {
			findings, err := m.Detector.Scan(ctx, texts)
			if err != nil {
				return
			}
			if cats := Categories(findings); len(cats) > 0 {
				observe(m.Detector.Name(), cats)
			}
		}(m)
	}
}
