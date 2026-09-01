package guardrails_test

import (
	"strings"
	"testing"

	"gatewai/gateway/internal/guardrails"
)

func TestMessageTexts_ExtractsToolCallArguments(t *testing.T) {
	body := []byte(`{"messages":[
		{"role":"user","content":"look up the user"},
		{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"lookup","arguments":"{\"email\":\"alice@example.com\"}"}}]}
	]}`)
	got := strings.Join(guardrails.MessageTexts(body), " | ")
	if !strings.Contains(got, "look up the user") {
		t.Errorf("user content missing: %q", got)
	}
	if !strings.Contains(got, "alice@example.com") {
		t.Errorf("tool_call arguments not scanned: %q", got)
	}
}

func TestResponseTexts_ExtractsToolCallArguments(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"content":null,"tool_calls":[{"type":"function","function":{"name":"send_email","arguments":"{\"to\":\"bob@example.com\"}"}}]}}]}`)
	got := strings.Join(guardrails.ResponseTexts(body), " | ")
	if !strings.Contains(got, "bob@example.com") {
		t.Errorf("response tool_call arguments not scanned: %q", got)
	}
}

func TestMessageTexts_NoToolCalls_StillWorks(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	got := guardrails.MessageTexts(body)
	if len(got) != 1 || got[0] != "hello" {
		t.Errorf("plain message extraction regressed: %v", got)
	}
}

func TestRedact_ToolCallArguments(t *testing.T) {
	// Assistant tool-call turn with content:null — must still redact args.
	body := []byte(`{"messages":[{"role":"assistant","content":null,"tool_calls":[{"function":{"name":"f","arguments":"{\"email\":\"bob@example.com\"}"}}]}]}`)
	out, cats := guardrails.New().Redact(body, []string{"pii"})
	if strings.Contains(string(out), "bob@example.com") {
		t.Fatalf("tool-call argument not redacted: %s", out)
	}
	if len(cats) == 0 {
		t.Error("expected a redacted category (email)")
	}
}

func TestRedactResponse_ToolCallArguments(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"function":{"name":"send","arguments":"{\"to\":\"bob@example.com\"}"}}]}}]}`)
	out, cats := guardrails.New().RedactResponse(body, []string{"pii"})
	if strings.Contains(string(out), "bob@example.com") {
		t.Fatalf("response tool-call argument not redacted: %s", out)
	}
	if len(cats) == 0 {
		t.Error("expected a redacted category (email)")
	}
}
