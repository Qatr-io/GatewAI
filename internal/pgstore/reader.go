package pgstore

import (
	"context"
	"time"
)

// UsageSummary is a rolled-up row for the self-service dashboard.
type UsageSummary struct {
	ServiceType           string
	Model                 string
	TotalRequests         int64
	TotalPromptTokens     int64
	TotalCompletionTokens int64
	AvgDurationMs         float64
	ErrorCount            int64
}

// RecentEvent is a single row for the history table.
type RecentEvent struct {
	ID               int64
	OccurredAt       time.Time
	EventType        string
	Consumer         string
	ServiceType      string
	Model            string
	PromptTokens     int
	CompletionTokens int
	HTTPStatus       int
	DurationMs       int64
	JobID            string
	JobStatus        string
	CacheHit         bool
}

// ConsumerSummary is used in the admin view.
type ConsumerSummary struct {
	Consumer          string
	TotalRequests     int64
	TotalTokens       int64
	LastSeenAt        time.Time
}

// QueryParams controls pagination and filtering for ListRecentEvents.
type QueryParams struct {
	Consumer    string    // empty = all (admin use)
	ServiceType string    // empty = all
	Since       time.Time // zero = last 30 days
	Until       time.Time // zero = now
	Limit       int       // default 50, max 200
	Offset      int
}

// GetUsageSummary returns per-service-type/model rolled-up stats for a consumer
// since the given time. Pass consumer="" only from admin paths.
func (s *Store) GetUsageSummary(ctx context.Context, consumer string, since time.Time) ([]UsageSummary, error) {
	if since.IsZero() {
		since = time.Now().UTC().Add(-30 * 24 * time.Hour)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT
			service_type,
			model,
			COUNT(*)                                           AS total_requests,
			COALESCE(SUM(prompt_tokens), 0)                   AS total_prompt_tokens,
			COALESCE(SUM(completion_tokens), 0)               AS total_completion_tokens,
			COALESCE(AVG(duration_ms) FILTER (WHERE duration_ms IS NOT NULL), 0) AS avg_duration_ms,
			COUNT(*) FILTER (WHERE http_status >= 400)        AS error_count
		FROM usage_events
		WHERE occurred_at >= $1
		  AND ($2 = '' OR consumer = $2)
		GROUP BY service_type, model
		ORDER BY total_requests DESC`,
		since, consumer,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []UsageSummary
	for rows.Next() {
		var r UsageSummary
		if err := rows.Scan(
			&r.ServiceType, &r.Model,
			&r.TotalRequests, &r.TotalPromptTokens, &r.TotalCompletionTokens,
			&r.AvgDurationMs, &r.ErrorCount,
		); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// ListRecentEvents returns a paginated list of events and the total count.
func (s *Store) ListRecentEvents(ctx context.Context, p QueryParams) ([]RecentEvent, int64, error) {
	if p.Limit <= 0 || p.Limit > 200 {
		p.Limit = 50
	}
	if p.Since.IsZero() {
		p.Since = time.Now().UTC().Add(-30 * 24 * time.Hour)
	}
	if p.Until.IsZero() {
		p.Until = time.Now().UTC()
	}

	var total int64
	err := s.pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM usage_events
		WHERE occurred_at BETWEEN $1 AND $2
		  AND ($3 = '' OR consumer = $3)
		  AND ($4 = '' OR service_type = $4)`,
		p.Since, p.Until, p.Consumer, p.ServiceType,
	).Scan(&total)
	if err != nil {
		return nil, 0, err
	}

	rows, err := s.pool.Query(ctx, `
		SELECT
			id, occurred_at, event_type, consumer, service_type, model,
			COALESCE(prompt_tokens, 0),
			COALESCE(completion_tokens, 0),
			COALESCE(http_status, 0),
			COALESCE(duration_ms, 0),
			COALESCE(job_id, ''),
			COALESCE(job_status, ''),
			COALESCE(cache_hit, false)
		FROM usage_events
		WHERE occurred_at BETWEEN $1 AND $2
		  AND ($3 = '' OR consumer = $3)
		  AND ($4 = '' OR service_type = $4)
		ORDER BY occurred_at DESC
		LIMIT $5 OFFSET $6`,
		p.Since, p.Until, p.Consumer, p.ServiceType, p.Limit, p.Offset,
	)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var events []RecentEvent
	for rows.Next() {
		var e RecentEvent
		if err := rows.Scan(
			&e.ID, &e.OccurredAt, &e.EventType, &e.Consumer, &e.ServiceType, &e.Model,
			&e.PromptTokens, &e.CompletionTokens, &e.HTTPStatus, &e.DurationMs,
			&e.JobID, &e.JobStatus, &e.CacheHit,
		); err != nil {
			return nil, 0, err
		}
		events = append(events, e)
	}
	return events, total, rows.Err()
}

// ListConsumerSummaries returns all consumers sorted by total requests (admin view).
func (s *Store) ListConsumerSummaries(ctx context.Context, since time.Time) ([]ConsumerSummary, error) {
	if since.IsZero() {
		since = time.Now().UTC().Add(-30 * 24 * time.Hour)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT
			consumer,
			COUNT(*)                                                    AS total_requests,
			COALESCE(SUM(prompt_tokens + completion_tokens), 0)        AS total_tokens,
			MAX(occurred_at)                                            AS last_seen_at
		FROM usage_events
		WHERE occurred_at >= $1
		  AND consumer != ''
		GROUP BY consumer
		ORDER BY total_requests DESC
		LIMIT 500`,
		since,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []ConsumerSummary
	for rows.Next() {
		var c ConsumerSummary
		if err := rows.Scan(&c.Consumer, &c.TotalRequests, &c.TotalTokens, &c.LastSeenAt); err != nil {
			return nil, err
		}
		result = append(result, c)
	}
	return result, rows.Err()
}
