package llmproxy

import (
	"bytes"
	"strings"
	"testing"

	"gatewai/gateway/internal/guardrails"
)

func feed(g *streamGuard, lines []string) {
	for _, l := range lines {
		if !g.line(l) {
			break
		}
	}
	g.finish()
}

func TestStreamGuard_RedactsEmailAcrossDeltas(t *testing.T) {
	var buf bytes.Buffer
	g := newStreamGuard(&buf, nil, guardrails.New(), []string{"pii"}, true, 64)
	feed(g, []string{
		`data: {"choices":[{"delta":{"role":"assistant"}}]}`, ``,
		`data: {"choices":[{"delta":{"content":"contact "}}]}`, ``,
		`data: {"choices":[{"delta":{"content":"bob@"}}]}`, ``,
		`data: {"choices":[{"delta":{"content":"example.com"}}]}`, ``,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`, ``,
		`data: [DONE]`, ``,
	})
	out := buf.String()
	if strings.Contains(out, "bob@example.com") {
		t.Errorf("email should have been redacted in stream: %s", out)
	}
	if !g.violated {
		t.Error("violated should be true after redacting a match")
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Errorf("[DONE] should pass through: %s", out)
	}
}

func TestStreamGuard_BlocksOnViolation(t *testing.T) {
	var buf bytes.Buffer
	g := newStreamGuard(&buf, nil, guardrails.New(), []string{"pii"}, false, 64)
	feed(g, []string{
		`data: {"choices":[{"delta":{"content":"reach me at bob@example.com now"}}]}`, ``,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`, ``,
		`data: [DONE]`, ``,
	})
	out := buf.String()
	if !g.aborted {
		t.Error("stream should have aborted on a block violation")
	}
	if !strings.Contains(out, "blocked by guardrails") {
		t.Errorf("expected a block error event: %s", out)
	}
	if strings.Contains(out, "bob@example.com") {
		t.Errorf("blocked content must not be forwarded: %s", out)
	}
}

func TestStreamGuard_CleanContentPassesThrough(t *testing.T) {
	var buf bytes.Buffer
	g := newStreamGuard(&buf, nil, guardrails.New(), []string{"pii"}, true, 64)
	feed(g, []string{
		`data: {"choices":[{"delta":{"content":"the weather is nice today"}}]}`, ``,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`, ``,
		`data: [DONE]`, ``,
	})
	out := buf.String()
	if g.violated {
		t.Error("clean content should not be flagged")
	}
	if !strings.Contains(out, "the weather is nice today") {
		t.Errorf("clean content should be forwarded: %s", out)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Error("[DONE] should pass through")
	}
}

func TestStreamGuard_WindowFlush(t *testing.T) {
	// A tiny window forces a flush mid-content (no control chunk needed).
	var buf bytes.Buffer
	g := newStreamGuard(&buf, nil, guardrails.New(), []string{"pii"}, true, 1) // 1 token → 4 chars
	feed(g, []string{
		`data: {"choices":[{"delta":{"content":"hello world this is long"}}]}`, ``,
	})
	if !strings.Contains(buf.String(), "hello") {
		t.Errorf("windowed content should have been emitted: %s", buf.String())
	}
}
