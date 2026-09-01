package guardrails_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"gatewai/gateway/internal/guardrails"
)

// TestSafeRedactPrefix_RuneBoundary asserts the incremental release cut always
// lands on a UTF-8 rune boundary, so a multi-byte character straddling the
// hold-back window is never split into an invalid byte sequence.
func TestSafeRedactPrefix_RuneBoundary(t *testing.T) {
	// Accented text: every "é"/"à"/"ç" is a 2-byte rune. Sweep the hold length so
	// the cut (len(text)-hold) lands on every possible byte offset, including the
	// interior of a multi-byte rune.
	text := "café à Noël déçu garçon éphémère"
	for hold := 0; hold <= len(text); hold++ {
		red, consumed, _ := guardrails.SafeRedactPrefix(text, []string{"pii"}, hold)
		if consumed < 0 || consumed > len(text) {
			t.Fatalf("hold=%d: consumed %d out of range", hold, consumed)
		}
		// The consumed prefix must be a whole number of runes (valid UTF-8).
		if !utf8.ValidString(text[:consumed]) {
			t.Fatalf("hold=%d: consumed prefix %q is not valid UTF-8", hold, text[:consumed])
		}
		if !utf8.ValidString(red) {
			t.Fatalf("hold=%d: redacted output %q is not valid UTF-8", hold, red)
		}
		if strings.ContainsRune(red, '�') {
			t.Fatalf("hold=%d: redacted output contains U+FFFD (rune split): %q", hold, red)
		}
	}
}

// TestSafeRedactPrefix_MultibyteBeforeMatch guards the specific straddle: a
// multi-byte rune immediately precedes an email that the hold window protects.
func TestSafeRedactPrefix_MultibyteBeforeMatch(t *testing.T) {
	text := "café bob@example.com"
	// Hold back enough to cover the email so its span is protected; the cut then
	// lands just before it, right after the accented word.
	red, consumed, _ := guardrails.SafeRedactPrefix(text, []string{"pii"}, len("bob@example.com"))
	if !utf8.ValidString(text[:consumed]) || !utf8.ValidString(red) {
		t.Fatalf("cut split a rune: consumed=%d red=%q", consumed, red)
	}
}
