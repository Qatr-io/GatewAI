package llmproxy

import (
	"encoding/json"
	"testing"
)

func fieldInt(t *testing.T, body []byte, field string) (int, bool) {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	v, ok := m[field]
	if !ok {
		return 0, false
	}
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("field %q not a number: %T", field, v)
	}
	return int(f), true
}

func TestClampOutputTokens(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		limit       int
		wantChanged bool
		wantMax     int // expected max_tokens (‑1 = must be absent)
		wantMaxComp int // expected max_completion_tokens (‑1 = must be absent)
	}{
		{"over cap clamps", `{"model":"m","max_tokens":4096}`, 256, true, 256, -1},
		{"at cap unchanged", `{"model":"m","max_tokens":256}`, 256, false, 256, -1},
		{"under cap unchanged", `{"model":"m","max_tokens":100}`, 256, false, 100, -1},
		{"omitted injects cap", `{"model":"m","messages":[]}`, 256, true, 256, -1},
		{"max_completion_tokens clamps", `{"model":"m","max_completion_tokens":9999}`, 256, true, -1, 256},
		{"both present, one over", `{"model":"m","max_tokens":10,"max_completion_tokens":9999}`, 256, true, 10, 256},
		{"limit zero is noop", `{"model":"m","max_tokens":4096}`, 0, false, 4096, -1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, changed := clampOutputTokens([]byte(tc.body), tc.limit)
			if changed != tc.wantChanged {
				t.Fatalf("changed=%v want %v (out=%s)", changed, tc.wantChanged, out)
			}
			if tc.wantMax == -1 {
				if _, ok := fieldInt(t, out, "max_tokens"); ok {
					t.Errorf("max_tokens should be absent: %s", out)
				}
			} else if got, _ := fieldInt(t, out, "max_tokens"); got != tc.wantMax {
				t.Errorf("max_tokens=%d want %d (%s)", got, tc.wantMax, out)
			}
			if tc.wantMaxComp == -1 {
				if _, ok := fieldInt(t, out, "max_completion_tokens"); ok {
					t.Errorf("max_completion_tokens should be absent: %s", out)
				}
			} else if got, _ := fieldInt(t, out, "max_completion_tokens"); got != tc.wantMaxComp {
				t.Errorf("max_completion_tokens=%d want %d (%s)", got, tc.wantMaxComp, out)
			}
		})
	}
}

func TestClampOutputTokens_InvalidJSONUnchanged(t *testing.T) {
	body := []byte(`not json`)
	out, changed := clampOutputTokens(body, 256)
	if changed || string(out) != string(body) {
		t.Fatalf("invalid JSON must pass through unchanged, got changed=%v out=%s", changed, out)
	}
}
