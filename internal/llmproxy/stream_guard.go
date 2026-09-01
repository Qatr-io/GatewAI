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
// Content deltas are accumulated in a buffer and released incrementally, always
// holding back a trailing window so a match that straddles a release boundary is
// never split (the reason a naive per-chunk scan leaks). Release differs by mode:
//   - buffer/redact: the largest prefix that does not cut through a match span is
//     redacted and emitted; the tail (< one window) is carried. At end-of-stream
//     the remainder is redacted and flushed.
//   - block: the whole buffer is scanned each release; any match terminates the
//     stream (nothing further is sent); otherwise the safe prefix is emitted
//     verbatim and the tail carried.
//
// Correctness holds for any match up to the window length; a pattern longer than
// the window (e.g. a large secret block) may have a prefix released before it
// completes — use a larger window or the non-streaming output stage for those.
//
// Non-content chunks (role, finish_reason, usage, tool_calls) pass through after
// the buffer is flushed. Content chunks are re-emitted as synthesized
// chat.completion.chunk deltas carrying the original id/model/created; a chunk
// that carries BOTH content and finish_reason has its content buffered and its
// finish_reason forwarded (content stripped) so the stop reason is never lost.
//
// NOTE: streamed tool-call argument deltas (delta.tool_calls) are treated as
// control and forwarded unscanned — streaming tool-call redaction is not yet
// covered (use the non-streaming path for tool-heavy guarded flows).
type streamGuard struct {
	w       io.Writer
	flusher http.Flusher
	checker *guardrails.Checker
	checks  []string
	redact  bool // true = buffer/redact, false = block
	window  int  // hold size in characters

	pending  string
	id       string // envelope fields carried from the backend's chunks
	model    string
	created  float64
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

// line processes one raw SSE line. Returns false when the stream must stop (a
// block violation, [DONE], or a client write error); the caller then breaks.
func (g *streamGuard) line(raw string) bool {
	if raw == "" {
		return true // SSE separator — synthesized chunks manage their own separators
	}
	data, isData := cutData(raw)
	if !isData {
		// Non-"data:" line (event:/id:/comment) — flush, then forward verbatim.
		if !g.emit(true) {
			return false
		}
		return g.write(raw + "\n\n")
	}
	if data == "[DONE]" {
		if !g.emit(true) {
			return false
		}
		g.write("data: [DONE]\n\n")
		return false
	}

	content, hasContent, hasFinish, rawNoContent := parseChunk(data)
	g.captureEnvelope(data)

	if !hasContent {
		// Control chunk (role/finish/usage/tool_calls) — flush, then forward.
		if !g.emit(true) {
			return false
		}
		return g.write("data: " + data + "\n\n")
	}

	g.pending += content
	if hasFinish {
		// Final content chunk: flush everything, then forward the finish_reason
		// (content stripped so it isn't duplicated).
		if !g.emit(true) {
			return false
		}
		return g.write("data: " + rawNoContent + "\n\n")
	}
	return g.emit(false)
}

// finish flushes any buffered content at end of stream.
func (g *streamGuard) finish() { g.emit(true) }

// emit releases buffered content. final=true finalizes everything; final=false
// releases only the safe prefix, holding a trailing window. Returns false when
// the stream must stop (block violation or write error).
func (g *streamGuard) emit(final bool) bool {
	if g.pending == "" {
		return true
	}
	if g.redact {
		hold := g.window
		if final {
			hold = 0
		}
		red, consumed, matched := guardrails.SafeRedactPrefix(g.pending, g.checks, hold)
		if consumed == 0 {
			return true // nothing safe to finalize yet
		}
		if matched {
			g.violated = true
		}
		g.pending = g.pending[consumed:]
		return g.writeContentChunk(red)
	}
	// block: any match anywhere in the buffer terminates the stream.
	if found := g.checker.ScanStrings([]string{g.pending}, g.checks); len(found) > 0 {
		g.violated = true
		g.aborted = true
		g.write(`data: {"error":"response blocked by guardrails"}` + "\n\n")
		g.write("data: [DONE]\n\n")
		return false
	}
	// Clean: release the safe prefix (verbatim), hold a trailing window.
	end := len(g.pending)
	if !final {
		end -= g.window
	}
	if end <= 0 {
		return true
	}
	out := g.pending[:end]
	g.pending = g.pending[end:]
	return g.writeContentChunk(out)
}

func (g *streamGuard) writeContentChunk(content string) bool {
	if content == "" {
		return true
	}
	delta := map[string]any{"content": content}
	choice := map[string]any{"index": 0, "delta": delta}
	chunk := map[string]any{"object": "chat.completion.chunk", "choices": []any{choice}}
	if g.id != "" {
		chunk["id"] = g.id
	}
	if g.model != "" {
		chunk["model"] = g.model
	}
	if g.created != 0 {
		chunk["created"] = g.created
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

// captureEnvelope records id/model/created from a chunk so synthesized content
// chunks carry the same identifiers (keeps strict OpenAI clients happy).
func (g *streamGuard) captureEnvelope(data string) {
	if g.id != "" {
		return
	}
	var e struct {
		ID      string  `json:"id"`
		Model   string  `json:"model"`
		Created float64 `json:"created"`
	}
	if json.Unmarshal([]byte(data), &e) == nil {
		g.id, g.model, g.created = e.ID, e.Model, e.Created
	}
}

// cutData strips the SSE "data:" prefix (with or without a following space) and
// returns the trimmed payload. ok=false for non-data lines.
func cutData(raw string) (string, bool) {
	if s, ok := strings.CutPrefix(raw, "data:"); ok {
		return strings.TrimSpace(s), true
	}
	return "", false
}

// parseChunk extracts choices[0].delta.content and whether the chunk carries a
// finish_reason. rawNoContent is the chunk re-serialized with delta.content
// removed (only computed when content and finish_reason coexist).
func parseChunk(data string) (content string, hasContent, hasFinish bool, rawNoContent string) {
	var obj map[string]any
	if json.Unmarshal([]byte(data), &obj) != nil {
		return "", false, false, data
	}
	choices, _ := obj["choices"].([]any)
	if len(choices) == 0 {
		return "", false, false, data
	}
	c0, _ := choices[0].(map[string]any)
	if c0 == nil {
		return "", false, false, data
	}
	if fr, ok := c0["finish_reason"]; ok && fr != nil {
		hasFinish = true
	}
	if delta, _ := c0["delta"].(map[string]any); delta != nil {
		if cv, ok := delta["content"].(string); ok && cv != "" {
			content, hasContent = cv, true
			if hasFinish {
				delete(delta, "content")
				if b, err := json.Marshal(obj); err == nil {
					rawNoContent = string(b)
				}
			}
		}
	}
	return content, hasContent, hasFinish, rawNoContent
}
