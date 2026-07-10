package usage

import "time"

// TotalUsage holds all-time (or retention-window) cumulative metrics.
type TotalUsage struct {
	Requests       int64       `json:"requests"`
	Jobs           int64       `json:"jobs,omitempty"`
	ProcessingTime float64     `json:"processing_time_seconds,omitempty"`
	Tokens         *TokenUsage `json:"tokens,omitempty"`
}

// TokenUsage holds cumulative LLM token counts.
type TokenUsage struct {
	Prompt     int64 `json:"prompt"`
	Completion int64 `json:"completion"`
}

// WindowUsage holds current-window metrics (rate-limit window).
type WindowUsage struct {
	Requests       int64      `json:"requests,omitempty"`
	Tokens         int64      `json:"tokens,omitempty"`
	ProcessingTime float64    `json:"processing_time_seconds,omitempty"`
	ResetAt        *time.Time `json:"reset_at,omitempty"`
}

// ServiceUsage holds usage data for one service type.
type ServiceUsage struct {
	ServiceType string       `json:"service_type"`
	Total       TotalUsage   `json:"total"`
	Window      *WindowUsage `json:"window,omitempty"`
}

// ConsumerUsage is the full usage response for one consumer.
type ConsumerUsage struct {
	Consumer   string         `json:"consumer"`
	Retention  string         `json:"retention"`
	LastActive *time.Time     `json:"last_active,omitempty"`
	Usage      []ServiceUsage `json:"usage"`
}

// AdminUsageResponse is the paginated admin response.
type AdminUsageResponse struct {
	Total     int64           `json:"total"`
	Limit     int64           `json:"limit"`
	Offset    int64           `json:"offset"`
	Consumers []ConsumerUsage `json:"consumers"`
}
