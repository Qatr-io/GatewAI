package guardrails_test

import (
	"sort"
	"testing"

	"gatewai/gateway/internal/guardrails"
)

func TestResultTexts(t *testing.T) {
	tests := []struct {
		name string
		body string
		want []string
	}{
		{"whisper transcript", `{"text":"call me","language":"en"}`, []string{"call me", "en"}},
		{"nested segments", `{"segments":[{"text":"hello"},{"text":"world"}]}`, []string{"hello", "world"}},
		{"blank strings skipped", `{"text":"  ","other":"x"}`, []string{"x"}},
		{"non-json body", "plain text body", []string{"plain text body"}},
		{"empty object", `{}`, nil},
		{"whitespace only", "   ", nil},
		{"numbers ignored", `{"n":42,"ok":true,"s":"keep"}`, []string{"keep"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := guardrails.ResultTexts([]byte(tt.body))
			// JSON object key order is non-deterministic; compare as multisets.
			sort.Strings(got)
			want := append([]string(nil), tt.want...)
			sort.Strings(want)
			if len(got) != len(want) {
				t.Fatalf("got %v, want %v", got, want)
			}
			for i := range got {
				if got[i] != want[i] {
					t.Fatalf("got %v, want %v", got, want)
				}
			}
		})
	}
}
