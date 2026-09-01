// Package guardrails provides multi-country PII and secrets detection for LLM
// request payloads, with support for scanning (detection only) and redaction
// (in-place replacement with placeholders). Patterns cover universal PII
// (email, credit card, IBAN, IPv4, international phone), country-specific PII
// for France, the United States, the United Kingdom, Spain, and Italy, as well
// as well-known secret formats (AWS keys, private keys, JWTs, and more).
//
// Note: purely numeric national-ID patterns (NIR, SIREN/SIRET, SSN, DNI) have
// higher false-positive rates than other patterns — enable per-service after
// assessing your payload characteristics.
package guardrails

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"
)

// Check group names used in the enabled filter of Scan and Redact.
const (
	CheckPII     = "pii"     // universal PII (email, credit card, IBAN, IPv4, international phone)
	CheckPIIFR   = "pii_fr"  // France-specific PII (phone, NIR, SIRET, SIREN)
	CheckPIIUS   = "pii_us"  // United States PII (SSN)
	CheckPIIUK   = "pii_uk"  // United Kingdom PII (NINO)
	CheckPIIES   = "pii_es"  // Spain PII (DNI)
	CheckPIIIT   = "pii_it"  // Italy PII (codice fiscale)
	CheckSecrets = "secrets" // API keys / tokens / private keys
)

type namedPattern struct {
	group       string
	name        string
	re          *regexp.Regexp
	placeholder string
}

var compiledPatterns = []namedPattern{
	// ── Universal PII ────────────────────────────────────────────────────────
	{
		group:       CheckPII,
		name:        "email",
		re:          regexp.MustCompile(`(?i)[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`),
		placeholder: "[REDACTED_EMAIL]",
	},
	{
		group: CheckPII,
		name:  "credit_card",
		// 13–16 digits, optionally space- or dash-separated.
		re:          regexp.MustCompile(`\b(?:\d[ \-]?){13,16}\b`),
		placeholder: "[REDACTED_CREDIT_CARD]",
	},
	{
		group:       CheckPII,
		name:        "iban",
		re:          regexp.MustCompile(`\b[A-Z]{2}\d{2}[A-Z0-9]{4}\d{7}[A-Z0-9]{0,16}\b`),
		placeholder: "[REDACTED_IBAN]",
	},
	{
		group:       CheckPII,
		name:        "ipv4",
		re:          regexp.MustCompile(`\b(?:(?:25[0-5]|2[0-4]\d|1?\d?\d)\.){3}(?:25[0-5]|2[0-4]\d|1?\d?\d)\b`),
		placeholder: "[REDACTED_IPV4]",
	},
	{
		group:       CheckPII,
		name:        "phone_intl",
		re:          regexp.MustCompile(`\+[1-9]\d{7,14}\b`),
		placeholder: "[REDACTED_PHONE_INTL]",
	},
	// ── France PII ───────────────────────────────────────────────────────────
	{
		group: CheckPIIFR,
		name:  "phone_fr",
		// +33, 0033, or 0 followed by a non-zero digit, groups of 2.
		re:          regexp.MustCompile(`(?:(?:\+|00)33|0)\s*[1-9](?:[\s.\-]*\d{2}){4}`),
		placeholder: "[REDACTED_PHONE_FR]",
	},
	{
		group:       CheckPIIFR,
		name:        "fr_nir",
		re:          regexp.MustCompile(`\b[12]\s?\d{2}\s?(?:0[1-9]|1[0-2])\s?\d{2}\s?\d{3}\s?\d{3}\s?\d{2}\b`),
		placeholder: "[REDACTED_FR_NIR]",
	},
	{
		group: CheckPIIFR,
		name:  "siret",
		// 14-digit SIRET checked before 9-digit SIREN to avoid double reporting.
		re:          regexp.MustCompile(`\b\d{14}\b`),
		placeholder: "[REDACTED_SIRET]",
	},
	{
		group:       CheckPIIFR,
		name:        "siren",
		re:          regexp.MustCompile(`\b\d{9}\b`),
		placeholder: "[REDACTED_SIREN]",
	},
	// ── United States PII ────────────────────────────────────────────────────
	{
		group:       CheckPIIUS,
		name:        "us_ssn",
		re:          regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`),
		placeholder: "[REDACTED_US_SSN]",
	},
	// ── United Kingdom PII ───────────────────────────────────────────────────
	{
		group:       CheckPIIUK,
		name:        "uk_nino",
		re:          regexp.MustCompile(`\b[A-CEGHJ-PR-TW-Z]{2}\s?\d{2}\s?\d{2}\s?\d{2}\s?[A-D]\b`),
		placeholder: "[REDACTED_UK_NINO]",
	},
	// ── Spain PII ────────────────────────────────────────────────────────────
	{
		group:       CheckPIIES,
		name:        "es_dni",
		re:          regexp.MustCompile(`\b\d{8}[A-HJ-NP-TV-Z]\b`),
		placeholder: "[REDACTED_ES_DNI]",
	},
	// ── Italy PII ────────────────────────────────────────────────────────────
	{
		group:       CheckPIIIT,
		name:        "it_codice_fiscale",
		re:          regexp.MustCompile(`\b[A-Z]{6}\d{2}[A-EHLMPR-T]\d{2}[A-Z]\d{3}[A-Z]\b`),
		placeholder: "[REDACTED_IT_CODICE_FISCALE]",
	},
	// ── Secrets ──────────────────────────────────────────────────────────────
	{
		group:       CheckSecrets,
		name:        "aws_access_key",
		re:          regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
		placeholder: "[REDACTED_AWS_ACCESS_KEY]",
	},
	{
		group:       CheckSecrets,
		name:        "private_key",
		re:          regexp.MustCompile(`-----BEGIN (?:RSA |EC |OPENSSH |DSA |PGP )?PRIVATE KEY-----`),
		placeholder: "[REDACTED_PRIVATE_KEY]",
	},
	{
		group:       CheckSecrets,
		name:        "jwt",
		re:          regexp.MustCompile(`eyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`),
		placeholder: "[REDACTED_JWT]",
	},
	{
		group:       CheckSecrets,
		name:        "github_token",
		re:          regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{36,}`),
		placeholder: "[REDACTED_GITHUB_TOKEN]",
	},
	{
		group:       CheckSecrets,
		name:        "slack_token",
		re:          regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{10,}`),
		placeholder: "[REDACTED_SLACK_TOKEN]",
	},
	{
		group:       CheckSecrets,
		name:        "google_api_key",
		re:          regexp.MustCompile(`AIza[0-9A-Za-z_-]{35}`),
		placeholder: "[REDACTED_GOOGLE_API_KEY]",
	},
}

// Checker scans OpenAI-compatible JSON message payloads for PII and secret
// patterns. A single Checker instance is safe for concurrent use.
type Checker struct{}

// New returns a ready-to-use Checker.
func New() *Checker { return &Checker{} }

// groupEnabled reports whether the given group name should be scanned given
// the enabled slice. An empty/nil enabled means all groups are active.
func groupEnabled(group string, enabled []string) bool {
	if len(enabled) == 0 {
		return true
	}
	for _, g := range enabled {
		if g == group {
			return true
		}
	}
	return false
}

// scanTexts is the shared matching core: given a slice of plain-text strings
// and an enabled-group filter, it returns the sorted, de-duplicated category
// names that match at least one pattern in an active group.
func scanTexts(texts []string, enabled []string) []string {
	if len(texts) == 0 {
		return nil
	}
	found := make(map[string]struct{})
	var violations []string
	for _, text := range texts {
		for _, p := range compiledPatterns {
			if !groupEnabled(p.group, enabled) {
				continue
			}
			if _, seen := found[p.name]; seen {
				continue
			}
			if p.re.MatchString(text) {
				found[p.name] = struct{}{}
				violations = append(violations, p.name)
			}
		}
	}
	return violations
}

// Scan returns the matched category names within the enabled check groups.
// enabled is a set of group names (e.g. []string{CheckPII, CheckSecrets});
// an empty/nil slice means ALL groups. Unknown group names are ignored.
// Returns nil when nothing matches or the body is not an OpenAI-compatible
// message payload.
func (c *Checker) Scan(body []byte, enabled []string) []string {
	return scanTexts(extractMessageTexts(body), enabled)
}

// ScanResponse returns matched category names found in an OpenAI-compatible
// LLM response body, restricted to the enabled groups (empty/nil = all).
// It inspects choices[*].message.content (string, or array-of-parts objects
// with a "text" field). Returns nil when nothing matches or the body is not
// a recognisable response.
func (c *Checker) ScanResponse(body []byte, enabled []string) []string {
	return scanTexts(extractResponseTexts(body), enabled)
}

// ScanStrings scans arbitrary text fragments (e.g. accumulated streaming
// delta content) restricted to enabled groups. Returns sorted,
// de-duplicated category names.
func (c *Checker) ScanStrings(texts []string, enabled []string) []string {
	return scanTexts(texts, enabled)
}

// Redact rewrites matched substrings inside message content with placeholders
// (e.g. "[REDACTED_EMAIL]"), restricted to the enabled groups. Returns the
// rewritten body (which remains valid JSON) and the sorted, de-duplicated list
// of category names that were redacted. On a non-message or unparseable body
// it returns (body, nil) unchanged.
func (c *Checker) Redact(body []byte, enabled []string) ([]byte, []string) {
	// Unmarshal into a generic map to preserve all fields.
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body, nil
	}

	msgsRaw, ok := payload["messages"]
	if !ok {
		return body, nil
	}
	msgs, ok := msgsRaw.([]any)
	if !ok || len(msgs) == 0 {
		return body, nil
	}

	redacted := make(map[string]struct{})

	for i, msgRaw := range msgs {
		msg, ok := msgRaw.(map[string]any)
		if !ok {
			continue
		}
		// content may be absent (assistant tool-call turns) — still scan tool_calls.
		if contentRaw, exists := msg["content"]; exists {
			switch cv := contentRaw.(type) {
			case string:
				newText, cats := applyRedactions(cv, enabled)
				for _, cat := range cats {
					redacted[cat] = struct{}{}
				}
				msg["content"] = newText

			case []any:
				for j, partRaw := range cv {
					part, ok := partRaw.(map[string]any)
					if !ok {
						continue
					}
					textRaw, hasText := part["text"]
					if !hasText {
						continue
					}
					textStr, ok := textRaw.(string)
					if !ok {
						continue
					}
					newText, cats := applyRedactions(textStr, enabled)
					for _, cat := range cats {
						redacted[cat] = struct{}{}
					}
					part["text"] = newText
					cv[j] = part
				}
				msg["content"] = cv
			}
		}
		redactToolCalls(msg, enabled, redacted)
		msgs[i] = msg
	}

	payload["messages"] = msgs

	if len(redacted) == 0 {
		return body, nil
	}

	out, err := json.Marshal(payload)
	if err != nil {
		return body, nil
	}

	categories := make([]string, 0, len(redacted))
	for cat := range redacted {
		categories = append(categories, cat)
	}
	sort.Strings(categories)

	return out, categories
}

// RedactResponse rewrites matches inside choices[*].message.content with
// placeholders (the same placeholders used by Redact), restricted to the
// enabled groups. Returns the rewritten body (valid JSON) and the sorted,
// de-duplicated categories redacted. On a non-response/unparseable body
// returns (body, nil) unchanged. The operation is idempotent.
func (c *Checker) RedactResponse(body []byte, enabled []string) ([]byte, []string) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body, nil
	}

	choicesRaw, ok := payload["choices"]
	if !ok {
		return body, nil
	}
	choices, ok := choicesRaw.([]any)
	if !ok || len(choices) == 0 {
		return body, nil
	}

	redacted := make(map[string]struct{})

	for i, choiceRaw := range choices {
		choice, ok := choiceRaw.(map[string]any)
		if !ok {
			continue
		}
		msgRaw, ok := choice["message"]
		if !ok {
			continue
		}
		msg, ok := msgRaw.(map[string]any)
		if !ok {
			continue
		}
		if contentRaw, exists := msg["content"]; exists {
			switch cv := contentRaw.(type) {
			case string:
				newText, cats := applyRedactions(cv, enabled)
				for _, cat := range cats {
					redacted[cat] = struct{}{}
				}
				msg["content"] = newText

			case []any:
				for j, partRaw := range cv {
					part, ok := partRaw.(map[string]any)
					if !ok {
						continue
					}
					textRaw, hasText := part["text"]
					if !hasText {
						continue
					}
					textStr, ok := textRaw.(string)
					if !ok {
						continue
					}
					newText, cats := applyRedactions(textStr, enabled)
					for _, cat := range cats {
						redacted[cat] = struct{}{}
					}
					part["text"] = newText
					cv[j] = part
				}
				msg["content"] = cv
			}
		}
		redactToolCalls(msg, enabled, redacted)
		choice["message"] = msg
		choices[i] = choice
	}

	payload["choices"] = choices

	if len(redacted) == 0 {
		return body, nil
	}

	out, err := json.Marshal(payload)
	if err != nil {
		return body, nil
	}

	categories := make([]string, 0, len(redacted))
	for cat := range redacted {
		categories = append(categories, cat)
	}
	sort.Strings(categories)

	return out, categories
}

// applyRedactions applies all active-group patterns to text and returns the
// rewritten string plus the list of category names that actually matched.
// redactToolCalls redacts matches inside each tool_calls[*].function.arguments
// string of an OpenAI message map (arguments is a JSON-encoded string). Matched
// categories are recorded into `into`. Mutates msg in place.
func redactToolCalls(msg map[string]any, enabled []string, into map[string]struct{}) {
	tcRaw, ok := msg["tool_calls"].([]any)
	if !ok {
		return
	}
	for k, tcAny := range tcRaw {
		tc, ok := tcAny.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := tc["function"].(map[string]any)
		if !ok {
			continue
		}
		argStr, ok := fn["arguments"].(string)
		if !ok || argStr == "" {
			continue
		}
		newArg, cats := applyRedactions(argStr, enabled)
		if len(cats) == 0 {
			continue
		}
		for _, cat := range cats {
			into[cat] = struct{}{}
		}
		fn["arguments"] = newArg
		tc["function"] = fn
		tcRaw[k] = tc
	}
	msg["tool_calls"] = tcRaw
}

// RedactText redacts matches of the enabled regex groups in a plain string,
// returning the redacted text and the categories that matched. Used for
// scanning already-extracted text such as buffered streaming content.
func RedactText(text string, enabled []string) (string, []string) {
	return applyRedactions(text, enabled)
}

// matchSpans returns the merged [start,end) byte ranges of all matches of the
// enabled groups in text (sorted, non-overlapping). Only complete matches are
// reported — an unfinished pattern at the very end of text produces no span.
func matchSpans(text string, enabled []string) [][2]int {
	var spans [][2]int
	for _, p := range compiledPatterns {
		if !groupEnabled(p.group, enabled) {
			continue
		}
		for _, loc := range p.re.FindAllStringIndex(text, -1) {
			spans = append(spans, [2]int{loc[0], loc[1]})
		}
	}
	if len(spans) < 2 {
		return spans
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i][0] < spans[j][0] })
	merged := spans[:1]
	for _, s := range spans[1:] {
		last := &merged[len(merged)-1]
		if s[0] <= last[1] {
			if s[1] > last[1] {
				last[1] = s[1]
			}
		} else {
			merged = append(merged, s)
		}
	}
	return merged
}

// SafeRedactPrefix supports incremental streaming redaction. It redacts the
// largest prefix of text that (a) leaves at least `hold` trailing characters
// unfinalized and (b) does not cut through a match span, and returns that
// redacted prefix, the number of ORIGINAL characters consumed (so the caller
// carries text[consumed:]), and whether anything was redacted. When nothing is
// safe to finalize yet (text no longer than `hold`, or a match straddles the
// boundary), consumed is 0. A `hold` of 0 finalizes everything (end of stream).
//
// Guarantees correct redaction for any match up to `hold` characters long; a
// match longer than `hold` may have a prefix finalized before it completes
// (inherent to windowed streaming — use a larger window or the non-streaming
// output stage for very long secrets).
func SafeRedactPrefix(text string, enabled []string, hold int) (redacted string, consumed int, matched bool) {
	boundary := len(text) - hold
	if boundary <= 0 {
		return "", 0, false
	}
	for _, sp := range matchSpans(text, enabled) {
		if sp[0] < boundary && boundary < sp[1] {
			boundary = sp[0] // don't finalize inside a match
		}
	}
	if boundary <= 0 {
		return "", 0, false
	}
	red, cats := applyRedactions(text[:boundary], enabled)
	return red, boundary, len(cats) > 0
}

func applyRedactions(text string, enabled []string) (string, []string) {
	var matched []string
	for _, p := range compiledPatterns {
		if !groupEnabled(p.group, enabled) {
			continue
		}
		if p.re.MatchString(text) {
			matched = append(matched, p.name)
			text = p.re.ReplaceAllString(text, p.placeholder)
		}
	}
	return text, matched
}

// extractResponseTexts pulls plaintext from the "choices[*].message.content"
// field of an OpenAI-compatible LLM response. Content may be a string or an
// array of content parts (each with a "text" field).
func extractResponseTexts(body []byte) []string {
	var payload struct {
		Choices []struct {
			Message struct {
				Content   json.RawMessage `json:"content"`
				ToolCalls []toolCall      `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || len(payload.Choices) == 0 {
		return nil
	}

	var texts []string
	for _, choice := range payload.Choices {
		texts = appendToolCallArgs(texts, choice.Message.ToolCalls)
		raw := choice.Message.Content
		if len(raw) == 0 {
			continue
		}
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			texts = append(texts, s)
			continue
		}
		var parts []struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(raw, &parts); err == nil {
			for _, p := range parts {
				if p.Text != "" {
					texts = append(texts, p.Text)
				}
			}
		}
	}
	return texts
}

// extractResultTexts collects every string leaf value from an arbitrary JSON
// result body. Async job results (transcripts, OCR, ...) have no fixed schema,
// so each string value is a candidate for content scanning. When the body is
// not JSON it is returned verbatim as a single fragment.
func extractResultTexts(body []byte) []string {
	var root any
	if err := json.Unmarshal(body, &root); err != nil {
		if s := strings.TrimSpace(string(body)); s != "" {
			return []string{s}
		}
		return nil
	}
	var texts []string
	var walk func(n any)
	walk = func(n any) {
		switch t := n.(type) {
		case string:
			if s := strings.TrimSpace(t); s != "" {
				texts = append(texts, s)
			}
		case []any:
			for _, e := range t {
				walk(e)
			}
		case map[string]any:
			for _, e := range t {
				walk(e)
			}
		}
	}
	walk(root)
	return texts
}

// RedactResultTexts redacts matches of the enabled regex groups inside every
// string leaf of a JSON result body, returning the rewritten body and the
// distinct categories redacted. A body that is not JSON is redacted as a single
// string. Used by the async result stage, whose payloads have no fixed schema.
// When nothing matched, the original body is returned unchanged.
func RedactResultTexts(body []byte, enabled []string) ([]byte, []string) {
	var root any
	if err := json.Unmarshal(body, &root); err != nil {
		red, matched := applyRedactions(string(body), enabled)
		if len(matched) == 0 {
			return body, nil
		}
		return []byte(red), matched
	}
	seen := map[string]bool{}
	var cats []string
	var walk func(n any) any
	walk = func(n any) any {
		switch t := n.(type) {
		case string:
			red, matched := applyRedactions(t, enabled)
			for _, c := range matched {
				if !seen[c] {
					seen[c] = true
					cats = append(cats, c)
				}
			}
			return red
		case []any:
			for i, e := range t {
				t[i] = walk(e)
			}
			return t
		case map[string]any:
			for k, e := range t {
				t[k] = walk(e)
			}
			return t
		default:
			return n
		}
	}
	root = walk(root)
	if len(cats) == 0 {
		return body, nil
	}
	out, err := json.Marshal(root)
	if err != nil {
		return body, nil
	}
	return out, cats
}

// extractMessageTexts pulls plaintext from the "messages[*].content" field
// of an OpenAI-compatible payload. Content may be a string or an array of
// content parts (each with a "text" field).
func extractMessageTexts(body []byte) []string {
	var payload struct {
		Messages []struct {
			Content   json.RawMessage `json:"content"`
			ToolCalls []toolCall      `json:"tool_calls"`
		} `json:"messages"`
		Prompt json.RawMessage `json:"prompt"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil
	}

	// Completions API (/v1/completions): a "prompt" string or array of strings.
	if len(payload.Messages) == 0 && len(payload.Prompt) > 0 {
		var s string
		if err := json.Unmarshal(payload.Prompt, &s); err == nil {
			if s == "" {
				return nil
			}
			return []string{s}
		}
		var arr []string
		if err := json.Unmarshal(payload.Prompt, &arr); err == nil {
			var texts []string
			for _, p := range arr {
				if p != "" {
					texts = append(texts, p)
				}
			}
			return texts
		}
		return nil
	}

	if len(payload.Messages) == 0 {
		return nil
	}

	var texts []string
	for _, msg := range payload.Messages {
		if len(msg.Content) == 0 {
			continue
		}
		// Try string content first (most common).
		var s string
		if err := json.Unmarshal(msg.Content, &s); err == nil {
			texts = append(texts, s)
			continue
		}
		// Try array of content parts (vision/multimodal payloads).
		var parts []struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(msg.Content, &parts); err == nil {
			for _, p := range parts {
				if p.Text != "" {
					texts = append(texts, p.Text)
				}
			}
		}
	}
	// Tool-call arguments carried in the message history (assistant turns) —
	// increasingly where real data flows, so scan them too.
	for _, msg := range payload.Messages {
		texts = appendToolCallArgs(texts, msg.ToolCalls)
	}
	return texts
}

// toolCall mirrors an OpenAI tool_call's function.arguments (a JSON string).
type toolCall struct {
	Function struct {
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// appendToolCallArgs appends each non-empty tool-call arguments string to texts.
func appendToolCallArgs(texts []string, calls []toolCall) []string {
	for _, c := range calls {
		if s := strings.TrimSpace(c.Function.Arguments); s != "" {
			texts = append(texts, s)
		}
	}
	return texts
}
