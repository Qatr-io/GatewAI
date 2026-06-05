package guardrails_test

import (
	"testing"

	"gatewai/gateway/internal/guardrails"
)

var checker = guardrails.New()

func TestCheck_NoViolations(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"What is the weather today?"}]}`)
	if v := checker.Check(body); len(v) != 0 {
		t.Errorf("expected no violations, got %v", v)
	}
}

func TestCheck_Email(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"Contact me at alice@example.com please."}]}`)
	assertViolation(t, body, "email")
}

func TestCheck_PhoneFR(t *testing.T) {
	cases := []string{
		`{"messages":[{"role":"user","content":"Appelez le 06 12 34 56 78"}]}`,
		`{"messages":[{"role":"user","content":"Tel: +33 1 23 45 67 89"}]}`,
		`{"messages":[{"role":"user","content":"0033612345678"}]}`,
	}
	for _, body := range cases {
		assertViolation(t, []byte(body), "phone_fr")
	}
}

func TestCheck_IBAN(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"Mon IBAN: FR7630006000011234567890189"}]}`)
	assertViolation(t, body, "iban")
}

func TestCheck_CreditCard(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"numéro de carte: 4111 1111 1111 1111"}]}`)
	assertViolation(t, body, "credit_card")
}

func TestCheck_SIRET(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"SIRET: 35219602400058"}]}`)
	assertViolation(t, body, "siret")
}

func TestCheck_MultipleViolations_DeduplicatedByCategory(t *testing.T) {
	body := []byte(`{"messages":[
		{"role":"user","content":"Contact alice@example.com or bob@example.com"},
		{"role":"assistant","content":"OK"}
	]}`)
	violations := checker.Check(body)
	count := 0
	for _, v := range violations {
		if v == "email" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("email category should appear once even with 2 matches, got %d times", count)
	}
}

func TestCheck_NotAMessagePayload_NoViolations(t *testing.T) {
	body := []byte(`{"prompt":"alice@example.com","max_tokens":100}`)
	if v := checker.Check(body); len(v) != 0 {
		t.Errorf("non-message payload should return nil, got %v", v)
	}
}

func TestCheck_ArrayContentParts(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"email: alice@example.com"}]}]}`)
	assertViolation(t, body, "email")
}

func TestCheck_EmptyBody_NoViolations(t *testing.T) {
	if v := checker.Check(nil); len(v) != 0 {
		t.Errorf("empty body should return nil, got %v", v)
	}
}

func assertViolation(t *testing.T, body []byte, want string) {
	t.Helper()
	violations := checker.Check(body)
	for _, v := range violations {
		if v == want {
			return
		}
	}
	t.Errorf("expected violation %q, got %v", want, violations)
}
