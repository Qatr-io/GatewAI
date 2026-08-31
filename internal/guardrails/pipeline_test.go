package guardrails_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"gatewai/gateway/internal/guardrails"
)

// fakeDetector implements guardrails.Detector deterministically for pipeline tests.
type fakeDetector struct {
	name     string
	findings []guardrails.Finding
	err      error
}

func (f fakeDetector) Name() string { return f.name }
func (f fakeDetector) Scan(context.Context, []string) ([]guardrails.Finding, error) {
	return f.findings, f.err
}
func (f fakeDetector) Redact(_ context.Context, body []byte) ([]byte, []guardrails.Finding, error) {
	return body, f.findings, f.err
}

func finding(cat string) guardrails.Finding {
	return guardrails.Finding{Category: cat, Detector: "fake", Score: 1}
}

func TestEvaluateSync_BlockAndFlag(t *testing.T) {
	models := []guardrails.Enforcement{
		{Detector: fakeDetector{name: "pg", findings: []guardrails.Finding{finding("injection")}}, Mode: guardrails.ModeSync, Action: guardrails.ActionBlock},
		{Detector: fakeDetector{name: "tox", findings: []guardrails.Finding{finding("toxicity")}}, Mode: guardrails.ModeSync, Action: guardrails.ActionFlag},
		{Detector: fakeDetector{name: "clean", findings: nil}, Mode: guardrails.ModeSync, Action: guardrails.ActionBlock},
		{Detector: fakeDetector{name: "shadow", findings: []guardrails.Finding{finding("x")}}, Mode: guardrails.ModeAsync, Action: guardrails.ActionFlag},
	}
	got := guardrails.EvaluateSync(context.Background(), models, []string{"text"})

	// Expect exactly the two sync detectors that fired (pg block, tox flag); the
	// clean detector and the async shadow are excluded.
	byName := map[string]guardrails.ModelResult{}
	for _, r := range got {
		byName[r.Name] = r
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 fired results, got %d: %+v", len(got), got)
	}
	if r, ok := byName["pg"]; !ok || r.Action != guardrails.ActionBlock || len(r.Categories) != 1 {
		t.Errorf("pg result wrong: %+v", r)
	}
	if r, ok := byName["tox"]; !ok || r.Action != guardrails.ActionFlag {
		t.Errorf("tox result wrong: %+v", r)
	}
	if _, ok := byName["shadow"]; ok {
		t.Error("async detector must not appear in EvaluateSync results")
	}
}

func TestEvaluateAll_RunsEveryModeIgnoringAction(t *testing.T) {
	models := []guardrails.Enforcement{
		{Detector: fakeDetector{name: "sync-det", findings: []guardrails.Finding{finding("pii")}}, Mode: guardrails.ModeSync, Action: guardrails.ActionBlock},
		{Detector: fakeDetector{name: "async-det", findings: []guardrails.Finding{finding("injection")}}, Mode: guardrails.ModeAsync, Action: guardrails.ActionFlag},
		{Detector: fakeDetector{name: "clean-det"}, Mode: guardrails.ModeAsync, Action: guardrails.ActionFlag},
	}
	got := guardrails.EvaluateAll(context.Background(), models, []string{"text"})
	if len(got) != 2 {
		t.Fatalf("want 2 fired results (both sync and async detectors), got %d: %+v", len(got), got)
	}
	names := map[string]bool{}
	for _, r := range got {
		names[r.Name] = true
	}
	if !names["sync-det"] || !names["async-det"] {
		t.Errorf("EvaluateAll must run detectors of every mode, got %v", names)
	}
	if names["clean-det"] {
		t.Error("a detector that found nothing must not appear in results")
	}
}

func TestEvaluateAll_Empty(t *testing.T) {
	if got := guardrails.EvaluateAll(context.Background(), nil, []string{"t"}); got != nil {
		t.Errorf("no models → nil, got %+v", got)
	}
	models := []guardrails.Enforcement{{Detector: fakeDetector{name: "d"}, Mode: guardrails.ModeAsync}}
	if got := guardrails.EvaluateAll(context.Background(), models, nil); got != nil {
		t.Errorf("no texts → nil, got %+v", got)
	}
}

func TestEvaluateAll_ReportsError(t *testing.T) {
	models := []guardrails.Enforcement{
		{Detector: fakeDetector{name: "boom", err: errors.New("down")}, Mode: guardrails.ModeAsync, Action: guardrails.ActionFlag},
	}
	got := guardrails.EvaluateAll(context.Background(), models, []string{"t"})
	if len(got) != 1 || got[0].Err == nil {
		t.Fatalf("errored detector must be reported with Err set, got %+v", got)
	}
}

func TestEvaluateSync_FailClosedError(t *testing.T) {
	models := []guardrails.Enforcement{
		{Detector: fakeDetector{name: "pg", err: errors.New("boom")}, Mode: guardrails.ModeSync, Action: guardrails.ActionBlock},
	}
	got := guardrails.EvaluateSync(context.Background(), models, []string{"t"})
	if len(got) != 1 || got[0].Err == nil || !got[0].Fired() {
		t.Fatalf("expected one fired error result, got %+v", got)
	}
}

func TestEvaluateSync_NoSyncModels(t *testing.T) {
	models := []guardrails.Enforcement{
		{Detector: fakeDetector{name: "shadow", findings: []guardrails.Finding{finding("x")}}, Mode: guardrails.ModeAsync, Action: guardrails.ActionFlag},
	}
	if got := guardrails.EvaluateSync(context.Background(), models, []string{"t"}); got != nil {
		t.Errorf("expected nil (no sync models), got %+v", got)
	}
}

func TestFireAsync_ObservesFiredOnly(t *testing.T) {
	models := []guardrails.Enforcement{
		{Detector: fakeDetector{name: "fires", findings: []guardrails.Finding{finding("injection")}}, Mode: guardrails.ModeAsync},
		{Detector: fakeDetector{name: "clean", findings: nil}, Mode: guardrails.ModeAsync},
		{Detector: fakeDetector{name: "errs", err: errors.New("x")}, Mode: guardrails.ModeAsync},
		{Detector: fakeDetector{name: "sync-ignored", findings: []guardrails.Finding{finding("y")}}, Mode: guardrails.ModeSync},
	}
	type obs struct {
		name string
		cats []string
	}
	ch := make(chan obs, 4)
	guardrails.FireAsync(context.Background(), models, []string{"t"}, func(name string, cats []string) {
		ch <- obs{name, cats}
	})

	// Only "fires" should observe. Collect for a short window.
	seen := map[string]bool{}
	deadline := time.After(500 * time.Millisecond)
loop:
	for {
		select {
		case o := <-ch:
			seen[o.name] = true
		case <-deadline:
			break loop
		}
	}
	if !seen["fires"] {
		t.Error("expected the firing async detector to be observed")
	}
	if seen["clean"] || seen["errs"] || seen["sync-ignored"] {
		t.Errorf("unexpected observations: %v", seen)
	}
}
