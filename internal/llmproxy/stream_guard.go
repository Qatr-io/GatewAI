package llmproxy

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"gatewai/gateway/internal/guardrails"
)

// streamGuard enforces output guardrails on a streaming (SSE) LLM response.
//
// Content deltas are held in a buffer instead of being forwarded immediately;
// once the buffer reaches the window size (or a sentence boundary, or a control
// chunk arrives) it is scanned and released:
//   - buffer/redact: matched spans are masked and the (possibly redacted) window
//     is emitted as a synthesized content chunk;
//   - block: on a violation the stream is terminated with an error event and no
//     further content is sent.
//
// Non-content chunks (role, finish_reason, usage, tool_calls) pass through
// unchanged after any pending buffer is flushed. Synthesized content chunks use
// a minimal OpenAI-compatible envelope (object=chat.completion.chunk,
// choices[0].delta.content), which streaming clients concatenate normally.
type streamGuard struct {
	w       io.Writer
	flusher http.Flusher
	checker *guardrails.Checker
	checks  []string
	redact  bool // true = buffer/redact, false = block
	window  int  // flush threshold in characters

	pending  strings.Builder
	aborted  bool // block violation → stream terminated
	violated bool // a match was seen (for metrics)
}

func newStreamGuard(w io.Writer, flusher http.Flusher, checker *guardrails.Checker, checks []string, redact bool, windowTokens int) *streamGuard {
	if windowTokens <= 0 {
		windowTokens = 64
	}
	return &streamGuard{
		w:       w,
		flusher: flusher,
		checker: checker,
		checks:  checks,
		redact:  redact,
		window:  windowTokens * 4, // ~4 chars/token
	}
}

// line processes one raw SSE line. It returns false when the stream must stop
// (a block violation, [DONE], or a client write error); the caller then breaks.
func (g *streamGuard) line(raw string) bool {
	if raw == "" {
		return true // SSE separator — synthesized chunks manage their own separators
	}
	after, ok := strings.CutPrefix(raw, "data: ")
	if ok && after != "[DONE]" {
		if content, isContent := deltaContent(after); isContent {
			g.pending.WriteString(content)
			if g.pending.Len() >= g.window || endsSentence(g.pending.String()) {
				return g.flush()
			}
			return true // buffered; not forwarded yet
		}
	}
	// Control line ([DONE], role/finish/usage/tool_calls chunk, or anything else):
	// flush what we have, then forward it verbatim.
	if !g.flush() {
		return false
	}
	if !g.write(raw + "\n\n") {
		return false
	}
	return raw != "data: [DONE]"
}

// finish flushes any buffered content at end of stream.
func (g *streamGuard) finish() {
	g.flush()
}

// flush scans and releases the buffered window. Returns false if the stream must
// stop (block violation or client write error).
func (g *streamGuard) flush() bool {
	if g.pending.Len() == 0 {
		return true
	}
	text := g.pending.String()
	g.pending.Reset()

	if g.redact {
		red, found := guardrails.RedactText(text, g.checks)
		if len(found) > 0 {
			g.violated = true
		}
		return g.writeContentChunk(red)
	}
	// block
	if found := g.checker.ScanStrings([]string{text}, g.checks); len(found) > 0 {
		g.violated = true
		g.aborted = true
		g.write(`data: {"error":"response blocked by guardrails"}` + "\n\n")
		g.write("data: [DONE]\n\n")
		return false
	}
	return g.writeContentChunk(text)
}

func (g *streamGuard) writeContentChunk(content string) bool {
	if content == "" {
		return true
	}
	chunk := map[string]any{
		"object": "chat.completion.chunk",
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{"content": content},
		}},
	}
	b, err := json.Marshal(chunk)
	if err != nil {
		return true
	}
	return g.write("data: " + string(b) + "\n\n")
}

func (g *streamGuard) write(s string) bool {
	if _, err := io.WriteString(g.w, s); err != nil {
		return false
	}
	if g.flusher != nil {
		g.flusher.Flush()
	}
	return true
}

// deltaContent returns choices[0].delta.content and whether it is a non-empty
// content delta (vs a role/finish/usage/tool_calls control chunk).
func deltaContent(data string) (string, bool) {
	var chunk struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(data), &chunk); err != nil || len(chunk.Choices) == 0 {
		return "", false
	}
	c := chunk.Choices[0].Delta.Content
	return c, c != ""
}

// endsSentence reports whether s ends at a natural boundary, so a window can be
// released cleanly rather than mid-word.
func endsSentence(s string) bool {
	s = strings.TrimRight(s, " \t")
	if s == "" {
		return false
	}
	switch s[len(s)-1] {
	case '.', '!', '?', '\n':
		return true
	}
	return false
}
