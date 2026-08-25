package consumer

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"gatewai/gateway/internal/config"
	"gatewai/gateway/internal/model"
)

// verifySignature mirrors what a webhook consumer would do.
func verifySignature(t *testing.T, header, secret string, body []byte, tolerance time.Duration) {
	t.Helper()
	var tsStr, v1 string
	for _, part := range strings.Split(header, ",") {
		if kv := strings.SplitN(part, "=", 2); len(kv) == 2 {
			switch kv[0] {
			case "t":
				tsStr = kv[1]
			case "v1":
				v1 = kv[1]
			}
		}
	}
	if tsStr == "" || v1 == "" {
		t.Fatalf("malformed signature header %q", header)
	}
	ts, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		t.Fatalf("bad timestamp: %v", err)
	}
	if d := time.Since(time.Unix(ts, 0)); d < -tolerance || d > tolerance {
		t.Fatalf("timestamp outside tolerance: %v", d)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(tsStr))
	mac.Write([]byte("."))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(v1)) {
		t.Fatalf("signature mismatch: got %s want %s", v1, expected)
	}
}

func TestWebhook_SignsWhenSecretSet(t *testing.T) {
	const secret = "whsec_test_123"
	var gotSig string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSig = r.Header.Get("X-Gatewai-Signature")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ws, _ := newTestSender(t, config.WebhookConfig{SigningSecret: secret})
	ws.Send(&model.Job{ID: "j1", ServiceType: "audio", Status: model.JobStatusCompleted, CallbackURL: srv.URL})

	if gotSig == "" {
		t.Fatal("expected X-Gatewai-Signature header, got none")
	}
	verifySignature(t, gotSig, secret, gotBody, time.Minute)
}

func TestWebhook_NoSignatureWhenSecretEmpty(t *testing.T) {
	var gotSig string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSig = r.Header.Get("X-Gatewai-Signature")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ws, _ := newTestSender(t, config.WebhookConfig{}) // no secret
	ws.Send(&model.Job{ID: "j2", ServiceType: "audio", Status: model.JobStatusCompleted, CallbackURL: srv.URL})

	if gotSig != "" {
		t.Fatalf("expected no signature header, got %q", gotSig)
	}
}
