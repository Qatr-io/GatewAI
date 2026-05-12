package adapter

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"kevent/relay/internal/config"
)

// Server hangs longer than the per-operation timeout → the per-call context
// must cancel and Call must return an error. Catches the regression we're
// fixing: a fixed http.Client.Timeout couldn't be raised for slow ops
// (pyannote diarization on long audio) without bloating the default for
// every other operation on the same relay binary.
func TestCall_PerOperationTimeoutFiresWhenServerHangs(t *testing.T) {
	hang := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-hang // wait until the client gives up
	}))
	defer func() {
		close(hang) // release the handler so the server can shut down
		srv.Close()
	}()

	cfg := &config.Config{
		Inference: config.InferenceConfig{
			BaseURL: srv.URL,
			Timeout: "10s", // would NOT save us here
			OperationTimeouts: map[string]string{
				"/v1/audio/diarizations": "50ms", // tight per-op timeout
			},
		},
	}
	a, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	start := time.Now()
	_, err = a.Call(context.Background(), CallInput{
		JobID:        "test-1",
		Filename:     "x.wav",
		Body:         strings.NewReader("audio bytes"),
		InferenceURL: "/v1/audio/diarizations",
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected timeout error, got nil")
	}
	// Should not hang anywhere near the global 10s; should bail at ~50ms.
	if elapsed > 2*time.Second {
		t.Fatalf("call took %v — per-operation timeout did not fire", elapsed)
	}
	// errors.Is on context.DeadlineExceeded is the canonical check.
	if !errors.Is(err, context.DeadlineExceeded) {
		// http.Client wraps the deadline in url.Error; check the message too.
		if !strings.Contains(err.Error(), "context deadline exceeded") {
			t.Fatalf("expected deadline exceeded, got: %v", err)
		}
	}
}

// When no per-operation override matches, the relay-wide default applies.
// Sanity test that we don't accidentally drop back to "no timeout".
func TestCall_FallsBackToDefaultTimeoutWhenNoOverride(t *testing.T) {
	hang := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-hang
	}))
	defer func() { close(hang); srv.Close() }()

	cfg := &config.Config{
		Inference: config.InferenceConfig{
			BaseURL: srv.URL,
			Timeout: "60ms", // default that should apply
			// no OperationTimeouts at all → exercise the nil-map path
		},
	}
	a, _ := New(cfg)

	start := time.Now()
	_, err := a.Call(context.Background(), CallInput{
		JobID:        "test-2",
		Filename:     "x.wav",
		Body:         strings.NewReader("audio bytes"),
		InferenceURL: "/v1/something/not-overridden",
	})
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("default timeout did not fire")
	}
}

// Happy-path : server responds quickly, no timeout fires, body is returned.
func TestCall_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Drain body to avoid the pipe writer hanging on Close.
		_, _ = http.MaxBytesReader(w, r.Body, 1<<20).Read(make([]byte, 1024))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	cfg := &config.Config{
		Inference: config.InferenceConfig{
			BaseURL: srv.URL,
			Timeout: "5s",
		},
	}
	a, _ := New(cfg)
	body, err := a.Call(context.Background(), CallInput{
		JobID:        "test-3",
		Filename:     "x.wav",
		Body:         strings.NewReader("audio"),
		InferenceURL: "/v1/audio/diarizations",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(string(body), `"ok":true`) {
		t.Fatalf("unexpected body: %s", body)
	}
}
