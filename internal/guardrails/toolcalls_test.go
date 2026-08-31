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
