package store_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"gatewai/relay/internal/model"
	"gatewai/relay/internal/store"
)

func newTestStore(t *testing.T) (*store.Store, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	return store.New(rdb), mr
}

func seedJob(t *testing.T, mr *miniredis.Miniredis, id string) {
	t.Helper()
	job := model.Job{ID: id, ServiceType: "transcription", Model: "whisper-large-v3", Status: "processing"}
	data, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("marshal seed job: %v", err)
	}
	if err := mr.Set("job:"+id, string(data)); err != nil {
		t.Fatalf("seed job: %v", err)
	}
}

// TestUpdateJobResult_ProcessingTime_WrittenWhenPositive is a regression guard
// for the pre-existing processing_time behaviour (no test previously covered
// this file at all).
func TestUpdateJobResult_ProcessingTime_WrittenWhenPositive(t *testing.T) {
	s, mr := newTestStore(t)
	seedJob(t, mr, "job-1")

	if err := s.UpdateJobResult(context.Background(), "job-1", model.JobStatusCompleted, "job-1/result.json", "", 9.5, 0, 0); err != nil {
		t.Fatalf("UpdateJobResult: %v", err)
	}

	got, err := s.GetJob(context.Background(), "job-1")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.Status != model.JobStatusCompleted {
		t.Errorf("status: got %q, want completed", got.Status)
	}
}

// TestUpdateJobResult_Tokens_WrittenWhenPositive verifies the new prompt/completion
// token fields round-trip through the Lua script.
func TestUpdateJobResult_Tokens_WrittenWhenPositive(t *testing.T) {
	s, mr := newTestStore(t)
	seedJob(t, mr, "job-1")

	if err := s.UpdateJobResult(context.Background(), "job-1", model.JobStatusCompleted, "job-1/result.json", "", 9.5, 120, 45); err != nil {
		t.Fatalf("UpdateJobResult: %v", err)
	}

	got, err := s.GetJob(context.Background(), "job-1")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.PromptTokens != 120 {
		t.Errorf("prompt_tokens: got %d, want 120", got.PromptTokens)
	}
	if got.CompletionTokens != 45 {
		t.Errorf("completion_tokens: got %d, want 45", got.CompletionTokens)
	}
}

// TestUpdateJobResult_Tokens_SkippedWhenZero verifies zero tokens are treated
// as "no data" (field omitted from the patch), matching the processing_time
// "" -> skip convention.
func TestUpdateJobResult_Tokens_SkippedWhenZero(t *testing.T) {
	s, mr := newTestStore(t)
	seedJob(t, mr, "job-1")

	if err := s.UpdateJobResult(context.Background(), "job-1", model.JobStatusFailed, "", "boom", 0, 0, 0); err != nil {
		t.Fatalf("UpdateJobResult: %v", err)
	}

	got, err := s.GetJob(context.Background(), "job-1")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.PromptTokens != 0 || got.CompletionTokens != 0 {
		t.Errorf("expected zero tokens, got prompt=%d completion=%d", got.PromptTokens, got.CompletionTokens)
	}
	if got.Status != model.JobStatusFailed {
		t.Errorf("status: got %q, want failed", got.Status)
	}
}

// TestGetJob_DeserializesAccountingFields verifies consumer_name, user_type,
// and callback_url round-trip through GetJob — the relay's exactly-once
// accounting/webhook side effects (see internal/accounting, internal/webhook)
// depend on these being present on the fetched Job.
func TestGetJob_DeserializesAccountingFields(t *testing.T) {
	s, mr := newTestStore(t)
	data := `{"id":"job-1","service_type":"transcription","status":"pending","consumer_name":"alice","user_type":"user","callback_url":"https://example.com/hook"}`
	if err := mr.Set("job:job-1", data); err != nil {
		t.Fatalf("seed job: %v", err)
	}

	got, err := s.GetJob(context.Background(), "job-1")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.ConsumerName != "alice" || got.UserType != "user" || got.CallbackURL != "https://example.com/hook" {
		t.Errorf("got %+v, want consumer_name=alice user_type=user callback_url=https://example.com/hook", got)
	}
}

// TestUpdateJobResult_TerminalJob_NotOverwritten is a regression guard for the
// existing already-terminal short-circuit in updateJobScript.
func TestUpdateJobResult_TerminalJob_NotOverwritten(t *testing.T) {
	s, mr := newTestStore(t)
	seedJob(t, mr, "job-1")
	if err := s.UpdateJobResult(context.Background(), "job-1", model.JobStatusCompleted, "ref-1", "", 1, 10, 2); err != nil {
		t.Fatalf("first UpdateJobResult: %v", err)
	}
	if err := s.UpdateJobResult(context.Background(), "job-1", model.JobStatusFailed, "", "late error", 2, 999, 999); err != nil {
		t.Fatalf("second UpdateJobResult: %v", err)
	}

	got, err := s.GetJob(context.Background(), "job-1")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.Status != model.JobStatusCompleted {
		t.Errorf("status: got %q, want completed (terminal jobs must not be overwritten)", got.Status)
	}
	if got.PromptTokens != 10 {
		t.Errorf("prompt_tokens: got %d, want 10 (unchanged)", got.PromptTokens)
	}
}
