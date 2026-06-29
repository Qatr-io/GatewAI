package pgstore

import (
	"context"
	"time"
)

// LLMEvent carries usage data captured after each LLM proxy response.
type LLMEvent struct {
	OccurredAt       time.Time
	Consumer         string
	UserType         string
	Subject          string
	ServiceType      string
	Model            string
	Provider         string
	PromptTokens     int
	CompletionTokens int
	HTTPStatus       int
	DurationMs       int64
	CacheHit         bool
}

// AsyncJobEvent carries usage data for job submission or completion.
type AsyncJobEvent struct {
	OccurredAt      time.Time
	EventType       string // "async_job_submitted" | "async_job_completed"
	Consumer        string
	UserType        string
	Subject         string
	ServiceType     string
	Model           string
	JobID           string
	JobStatus       string
	ProcessingTimeS float64
}

// WriteLLMEvent persists an LLM usage event.
func (s *Store) WriteLLMEvent(ctx context.Context, e LLMEvent) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO usage_events
		  (event_type, occurred_at, consumer, user_type, subject,
		   service_type, model, provider,
		   prompt_tokens, completion_tokens, http_status, duration_ms, cache_hit)
		VALUES
		  ('llm', $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		e.OccurredAt, e.Consumer, e.UserType, e.Subject,
		e.ServiceType, e.Model, e.Provider,
		nullInt(e.PromptTokens), nullInt(e.CompletionTokens),
		e.HTTPStatus, e.DurationMs, e.CacheHit,
	)
	return err
}

// WriteAsyncJobEvent persists an async job submission or completion event.
func (s *Store) WriteAsyncJobEvent(ctx context.Context, e AsyncJobEvent) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO usage_events
		  (event_type, occurred_at, consumer, user_type, subject,
		   service_type, model, job_id, job_status, processing_time_s)
		VALUES
		  ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		e.EventType, e.OccurredAt, e.Consumer, e.UserType, e.Subject,
		e.ServiceType, e.Model, e.JobID, e.JobStatus,
		nullFloat(e.ProcessingTimeS),
	)
	return err
}

// nullInt returns nil for 0, preserving the semantic "no tokens" vs "zero tokens".
func nullInt(v int) any {
	if v == 0 {
		return nil
	}
	return v
}

func nullFloat(v float64) any {
	if v == 0 {
		return nil
	}
	return v
}
