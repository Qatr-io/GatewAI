package guardrails_test

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"

	"gatewai/gateway/internal/guardrails"
)

var checker = guardrails.New()

// ── Scan: one category per group ─────────────────────────────────────────────

func TestScan_PII_Group(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"Email me at bob@example.org"}]}`)
	assertViolation(t, checker.Scan(body, []string{guardrails.CheckPII}), "email")
}

func TestScan_PIIFR_Group(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"SIRET: 35219602400058"}]}`)
	assertViolation(t, checker.Scan(body, []string{guardrails.CheckPIIFR}), "siret")
}

func TestScan_PIIUS_Group(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"SSN: 123-45-6789"}]}`)
	assertViolation(t, checker.Scan(body, []string{guardrails.CheckPIIUS}), "us_ssn")
}

func TestScan_PIIUK_Group(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"NINO: AB 12 34 56 A"}]}`)
	assertViolation(t, checker.Scan(body, []string{guardrails.CheckPIIUK}), "uk_nino")
}

func TestScan_PIIES_Group(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"DNI: 12345678Z"}]}`)
	assertViolation(t, checker.Scan(body, []string{guardrails.CheckPIIES}), "es_dni")
}

func TestScan_PIIES_InvalidCheckChar(t *testing.T) {
	// 'I' is excluded from the DNI check character set [A-HJ-NP-TV-Z]
	body := []byte(`{"messages":[{"role":"user","content":"bad: 12345678I"}]}`)
	got := checker.Scan(body, []string{guardrails.CheckPIIES})
	for _, v := range got {
		if v == "es_dni" {
			t.Errorf("12345678I should not match DNI (I is excluded)")
		}
	}
}

func TestScan_PIIT_Group(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"CF: RSSMRA85T10A562S"}]}`)
	assertViolation(t, checker.Scan(body, []string{guardrails.CheckPIIIT}), "it_codice_fiscale")
}

func TestScan_Secrets_Group(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"key: AKIAIOSFODNN7EXAMPLE123"}]}`)
	assertViolation(t, checker.Scan(body, []string{guardrails.CheckSecrets}), "aws_access_key")
}

// ── Scan: group selection filtering ──────────────────────────────────────────

func TestScan_OnlySecrets_DoesNotReportPIIFR(t *testing.T) {
	// contains a French SIRET but should only be checked for secrets
	body := []byte(`{"messages":[{"role":"user","content":"SIRET 35219602400058 key AKIAIOSFODNN7EXAMPLE123"}]}`)
	got := checker.Scan(body, []string{guardrails.CheckSecrets})
	for _, v := range got {
		if v == "siret" {
			t.Errorf("expected no pii_fr results when enabled=[secrets], got siret")
		}
	}
	assertViolation(t, got, "aws_access_key")
}

func TestScan_OnlyPIIUS_DoesNotReportPIIFR(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"SSN: 123-45-6789 tel 06 12 34 56 78"}]}`)
	got := checker.Scan(body, []string{guardrails.CheckPIIUS})
	assertViolation(t, got, "us_ssn")
	for _, v := range got {
		if v == "phone_fr" {
			t.Errorf("pii_fr should not be reported when enabled=[pii_us]")
		}
	}
}

func TestScan_NilEnabled_AllGroups(t *testing.T) {
	// Contains pii (email), pii_us (ssn), secrets (aws key)
	body := []byte(`{"messages":[{"role":"user","content":"alice@example.com SSN 123-45-6789 AKIAIOSFODNN7EXAMPLE123"}]}`)
	got := checker.Scan(body, nil)
	assertViolation(t, got, "email")
	assertViolation(t, got, "us_ssn")
	assertViolation(t, got, "aws_access_key")
}

// ── Redact ────────────────────────────────────────────────────────────────────

func TestRedact_StringContent_MultiGroup(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"email: alice@example.com IBAN: FR7630006000011234567890189 key: AKIAIOSFODNN7EXAMPLE123 SSN: 123-45-6789"}],"model":"gpt-4"}`)
	out, cats := checker.Redact(body, nil)

	// Must be valid JSON.
	if !json.Valid(out) {
		t.Fatalf("Redact returned invalid JSON: %s", out)
	}

	wantCats := []string{"aws_access_key", "email", "iban", "us_ssn"}
	sort.Strings(cats)
	if !reflect.DeepEqual(cats, wantCats) {
		t.Errorf("expected categories %v, got %v", wantCats, cats)
	}

	// Redacted content must not contain originals.
	content := extractFirstContent(t, out)
	assertNotContains(t, content, "alice@example.com")
	assertNotContains(t, content, "FR7630006000011234567890189")
	assertNotContains(t, content, "AKIAIOSFODNN7EXAMPLE123")
	assertNotContains(t, content, "123-45-6789")

	// Must contain placeholders.
	assertContains(t, content, "[REDACTED_EMAIL]")
	assertContains(t, content, "[REDACTED_IBAN]")
	assertContains(t, content, "[REDACTED_AWS_ACCESS_KEY]")
	assertContains(t, content, "[REDACTED_US_SSN]")
}

func TestRedact_ArrayOfParts(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"my email alice@example.com"},{"type":"image_url","image_url":"http://example.com/img.png"}]}]}`)
	out, cats := checker.Redact(body, []string{guardrails.CheckPII})

	if !json.Valid(out) {
		t.Fatalf("Redact returned invalid JSON: %s", out)
	}
	assertViolation(t, cats, "email")

	// The image_url part must be preserved unchanged.
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatal(err)
	}
	msgs := payload["messages"].([]any)
	msg := msgs[0].(map[string]any)
	parts := msg["content"].([]any)
	if len(parts) != 2 {
		t.Fatalf("expected 2 content parts, got %d", len(parts))
	}
	imgPart := parts[1].(map[string]any)
	if imgPart["image_url"] != "http://example.com/img.png" {
		t.Errorf("image_url part was modified: %v", imgPart)
	}
}

func TestRedact_PreservesUnrelatedFields(t *testing.T) {
	body := []byte(`{"model":"gpt-4","temperature":0.7,"messages":[{"role":"user","content":"hello world"}]}`)
	out, cats := checker.Redact(body, nil)

	if cats != nil {
		t.Errorf("expected no redactions, got %v", cats)
	}
	// Output should equal input (no redactions happened, body returned unchanged).
	if string(out) != string(body) {
		t.Errorf("body changed despite no redactions:\n  in:  %s\n  out: %s", body, out)
	}

	// Verify fields preserved in a round-trip redaction scenario too.
	body2 := []byte(`{"model":"gpt-4","temperature":0.7,"messages":[{"role":"user","content":"alice@example.com"}]}`)
	out2, _ := checker.Redact(body2, nil)
	var m map[string]any
	if err := json.Unmarshal(out2, &m); err != nil {
		t.Fatal(err)
	}
	if m["model"] != "gpt-4" {
		t.Errorf("model field lost after redaction: %v", m["model"])
	}
	if m["temperature"].(float64) != 0.7 {
		t.Errorf("temperature field lost after redaction: %v", m["temperature"])
	}
}

func TestRedact_Idempotent(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"alice@example.com SSN 123-45-6789"}]}`)
	out1, cats1 := checker.Redact(body, nil)
	out2, cats2 := checker.Redact(out1, nil)

	if string(out1) != string(out2) {
		t.Errorf("Redact is not idempotent:\n  pass1: %s\n  pass2: %s", out1, out2)
	}
	// Second pass should find nothing new (placeholders don't match patterns).
	if cats2 != nil {
		t.Errorf("second Redact pass should return nil categories, got %v", cats2)
	}
	_ = cats1
}

func TestRedact_NonMessageBody_Unchanged(t *testing.T) {
	body := []byte(`{"foo":"alice@example.com"}`)
	out, cats := checker.Redact(body, nil)
	if string(out) != string(body) {
		t.Errorf("non-message body should be returned unchanged, got %s", out)
	}
	if cats != nil {
		t.Errorf("expected nil categories for non-message body, got %v", cats)
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

func assertViolation(t *testing.T, violations []string, want string) {
	t.Helper()
	for _, v := range violations {
		if v == want {
			return
		}
	}
	t.Errorf("expected violation %q, got %v", want, violations)
}

func assertContains(t *testing.T, s, substr string) {
	t.Helper()
	if len(s) == 0 || len(substr) == 0 {
		return
	}
	if !containsStr(s, substr) {
		t.Errorf("expected %q to contain %q", s, substr)
	}
}

func assertNotContains(t *testing.T, s, substr string) {
	t.Helper()
	if containsStr(s, substr) {
		t.Errorf("expected %q NOT to contain %q", s, substr)
	}
}

func containsStr(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && searchStr(s, substr))
}

func searchStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// extractFirstContent extracts the string "content" from the first message.
func extractFirstContent(t *testing.T, body []byte) string {
	t.Helper()
	var payload struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || len(payload.Messages) == 0 {
		t.Fatalf("could not parse output body: %v", err)
	}
	return payload.Messages[0].Content
}
