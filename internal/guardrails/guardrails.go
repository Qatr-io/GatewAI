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

// Scan returns the matched category names within the enabled check groups.
// enabled is a set of group names (e.g. []string{CheckPII, CheckSecrets});
// an empty/nil slice means ALL groups. Unknown group names are ignored.
// Returns nil when nothing matches or the body is not an OpenAI-compatible
// message payload.
func (c *Checker) Scan(body []byte, enabled []string) []string {
	texts := extractMessageTexts(body)
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
		contentRaw, exists := msg["content"]
		if !exists {
			continue
		}

		switch cv := contentRaw.(type) {
		case string:
			newText, cats := applyRedactions(cv, enabled)
			for _, cat := range cats {
				redacted[cat] = struct{}{}
			}
			msg["content"] = newText
			msgs[i] = msg

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
			msgs[i] = msg
		}
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

// applyRedactions applies all active-group patterns to text and returns the
// rewritten string plus the list of category names that actually matched.
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

// extractMessageTexts pulls plaintext from the "messages[*].content" field
// of an OpenAI-compatible payload. Content may be a string or an array of
// content parts (each with a "text" field).
func extractMessageTexts(body []byte) []string {
	var payload struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || len(payload.Messages) == 0 {
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
	return texts
}
