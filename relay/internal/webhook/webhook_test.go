package webhook_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gatewai/relay/internal/model"
	"gatewai/relay/internal/webhook"
)

type mockObjectStore struct {
	getBody   string
	getErr    error
	deleted   []string
	deleteErr error
}

func (m *mockObjectStore) GetObject(_ context.Context, _ string) (io.ReadCloser, int64, string, error) {
	if m.getErr != nil {
		return nil, 0, "", m.getErr
	}
	return io.NopCloser(strings.NewReader(m.getBody)), int64(len(m.getBody)), "application/json", nil
}

func (m *mockObjectStore) DeleteObject(_ context.Context, key string) error {
	m.deleted = append(m.deleted, key)
	return m.deleteErr
}

// webhookPayload mirrors the unexported type in the webhook package for test
// deserialization (fields must match json tags exactly).
type webhookPayload struct {
	JobID       string          `json:"job_id"`
	ServiceType string          `json:"service_type"`
	Status      string          `json:"status"`
	Result      json.RawMessage `json:"result,omitempty"`
	Error       string          `json:"error,omitempty"`
	CompletedAt time.Time       `json:"completed_at"`
}

func TestSend_DeliversCompletedPayloadWithResult(t *testing.T) {
	var received webhookPayload
	var gotJobIDHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotJobIDHeader = r.Header.Get("X-Job-ID")
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &received)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	job := &model.Job{ID: "job-1", ServiceType: "transcription", CallbackURL: srv.URL}
	s3 := &mockObjectStore{getBody: `{"text":"hello"}`}

	webhook.Send(context.Background(), job, model.JobStatusCompleted, "job-1/result.json", "", s3, srv.Client(), false)

	if gotJobIDHeader != "job-1" {
		t.Errorf("X-Job-ID header: got %q, want job-1", gotJobIDHeader)
	}
	if received.JobID != "job-1" || received.Status != "completed" {
		t.Errorf("payload: got %+v", received)
	}
	if string(received.Result) != `{"text":"hello"}` {
		t.Errorf("result: got %s", received.Result)
	}
}

func TestSend_DeletesResultAfterDelivery_WhenNotPersisted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	job := &model.Job{ID: "job-1", ServiceType: "transcription", CallbackURL: srv.URL}
	s3 := &mockObjectStore{getBody: `{}`}

	webhook.Send(context.Background(), job, model.JobStatusCompleted, "job-1/result.json", "", s3, srv.Client(), false)

	if len(s3.deleted) != 1 || s3.deleted[0] != "job-1/result.json" {
		t.Errorf("expected result deleted, got %v", s3.deleted)
	}
}

func TestSend_KeepsResult_WhenPersistsResultTrue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	job := &model.Job{ID: "job-1", ServiceType: "transcription", CallbackURL: srv.URL}
	s3 := &mockObjectStore{getBody: `{}`}

	webhook.Send(context.Background(), job, model.JobStatusCompleted, "job-1/result.json", "", s3, srv.Client(), true)

	if len(s3.deleted) != 0 {
		t.Errorf("expected no deletion when persists_result=true, got %v", s3.deleted)
	}
}

func TestSend_FailedJob_NoResultFetch(t *testing.T) {
	var received webhookPayload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &received)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	job := &model.Job{ID: "job-2", ServiceType: "transcription", CallbackURL: srv.URL}
	s3 := &mockObjectStore{}

	webhook.Send(context.Background(), job, model.JobStatusFailed, "", "inference: timeout", s3, srv.Client(), false)

	if received.Status != "failed" || received.Error != "inference: timeout" {
		t.Errorf("got %+v", received)
	}
	if received.Result != nil {
		t.Errorf("expected no result for failed job, got %s", received.Result)
	}
}

func TestSend_RetriesOnServerError(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n < 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	job := &model.Job{ID: "job-3", ServiceType: "transcription", CallbackURL: srv.URL}
	s3 := &mockObjectStore{}

	start := time.Now()
	webhook.Send(context.Background(), job, model.JobStatusFailed, "", "boom", s3, srv.Client(), false)
	elapsed := time.Since(start)

	if attempts.Load() != 2 {
		t.Errorf("expected 2 attempts, got %d", attempts.Load())
	}
	if elapsed < 2*time.Second {
		t.Errorf("expected at least the 2s initial backoff between attempts, elapsed %v", elapsed)
	}
}
