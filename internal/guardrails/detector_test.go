package guardrails_test

import (
	"context"
	"reflect"
	"testing"

	"gatewai/gateway/internal/guardrails"
)

// Compile-time assertion that RegexDetector satisfies the Detector interface.
var _ guardrails.Detector = (*guardrails.RegexDetector)(nil)

func TestRegexDetector_Name(t *testing.T) {
	if got := guardrails.NewRegexDetector().Name(); got != "regex" {
		t.Errorf("Name() = %q, want %q", got, "regex")
	}
}

// parityCases exercises the same bodies/groups the Checker tests use, so the
// adapter is proven to change nothing.
var parityCases = []struct {
	name   string
	body   string
	groups []string
}{
	{"email", `{"messages":[{"role":"user","content":"Email me at bob@example.org"}]}`, []string{guardrails.CheckPII}},
	{"siret", `{"messages":[{"role":"user","content":"SIRET: 35219602400058"}]}`, []string{guardrails.CheckPIIFR}},
	{"ssn", `{"messages":[{"role":"user","content":"SSN: 123-45-6789"}]}`, []string{guardrails.CheckPIIUS}},
	{"aws", `{"messages":[{"role":"user","content":"key: AKIAIOSFODNN7EXAMPLE123"}]}`, []string{guardrails.CheckSecrets}},
	{"multi", `{"messages":[{"role":"user","content":"SIRET 35219602400058 key AKIAIOSFODNN7EXAMPLE123"}]}`, []string{guardrails.CheckPIIFR, guardrails.CheckSecrets}},
	{"all-groups", `{"messages":[{"role":"user","content":"Email bob@example.org SSN 123-45-6789"}]}`, nil},
	{"no-match", `{"messages":[{"role":"user","content":"just a harmless sentence"}]}`, []string{guardrails.CheckPII}},
	{"not-a-message", `{"foo":"bar"}`, []string{guardrails.CheckPII}},
}

func TestRegexDetector_Scan_ParityWithChecker(t *testing.T) {
	c := guardrails.New()
	for _, tc := range parityCases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(tc.body)
			want := c.Scan(body, tc.groups)

			det := guardrails.NewRegexDetector(tc.groups...)
			findings, err := det.Scan(context.Background(), guardrails.MessageTexts(body))
			if err != nil {
				t.Fatalf("Scan error: %v", err)
			}
			got := guardrails.Categories(findings)

			if !reflect.DeepEqual(got, want) {
				t.Errorf("categories mismatch:\n  detector: %v\n  checker:  %v", got, want)
			}
			for _, f := range findings {
				if f.Detector != "regex" {
					t.Errorf("finding %q has detector %q, want regex", f.Category, f.Detector)
				}
				if f.Score != 1.0 {
					t.Errorf("finding %q has score %v, want 1.0", f.Category, f.Score)
				}
			}
		})
	}
}

func TestRegexDetector_Redact_ParityWithChecker(t *testing.T) {
	c := guardrails.New()
	for _, tc := range parityCases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(tc.body)
			wantBody, wantCats := c.Redact(body, tc.groups)

			det := guardrails.NewRegexDetector(tc.groups...)
			gotBody, findings, err := det.Redact(context.Background(), body)
			if err != nil {
				t.Fatalf("Redact error: %v", err)
			}
			if string(gotBody) != string(wantBody) {
				t.Errorf("redacted body mismatch:\n  detector: %s\n  checker:  %s", gotBody, wantBody)
			}
			if got := guardrails.Categories(findings); !reflect.DeepEqual(got, wantCats) {
				t.Errorf("redacted categories mismatch:\n  detector: %v\n  checker:  %v", got, wantCats)
			}
		})
	}
}

func TestCategories_Empty(t *testing.T) {
	if got := guardrails.Categories(nil); got != nil {
		t.Errorf("Categories(nil) = %v, want nil", got)
	}
}
