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

// WindowUsage holds current-window metrics (rate-limit window) alongside the
// configured quota (from rate_limits in config.yaml) that usage is measured against.
type WindowUsage struct {
	Requests       int64      `json:"requests,omitempty"`
	Tokens         int64      `json:"tokens,omitempty"`
	ProcessingTime float64    `json:"processing_time_seconds,omitempty"`
	ResetAt        *time.Time `json:"reset_at,omitempty"`

	RequestLimit         int64  `json:"request_limit,omitempty"`
	RequestPeriod        string `json:"request_period,omitempty"`
	TokenLimit           int64  `json:"token_limit,omitempty"`
	TokenPeriod          string `json:"token_period,omitempty"`
	ProcessingTimeLimit  int64  `json:"processing_time_limit_seconds,omitempty"`
	ProcessingTimePeriod string `json:"processing_time_period,omitempty"`
}

// ModelUsage holds per-model token usage and quota, for services that
// configure a model-level token budget (services[].token_limits), nested
// under the owning ServiceUsage entry.
type ModelUsage struct {
	Model       string     `json:"model"`
	Tokens      int64      `json:"tokens,omitempty"`
	TokenLimit  int64      `json:"token_limit,omitempty"`
	TokenPeriod string     `json:"token_period,omitempty"`
	ResetAt     *time.Time `json:"reset_at,omitempty"`
}

// ServiceUsage holds usage data for one service type.
type ServiceUsage struct {
	ServiceType string       `json:"service_type"`
	Total       TotalUsage   `json:"total"`
	Window      *WindowUsage `json:"window,omitempty"`
	Models      []ModelUsage `json:"models,omitempty"`
}

// ConsumerUsage is the full usage response for one consumer.
type ConsumerUsage struct {
	Consumer   string         `json:"consumer"`
	Retention  string         `json:"retention"`
	LastActive *time.Time     `json:"last_active,omitempty"`
	Usage      []ServiceUsage `json:"usage"`

	// UserType is the value resolved from server.user_type_header for this
	// request (or "*" when unset/absent), i.e. the tier used to look up
	// rate_limits/token_limits quotas above. Surfaced so callers can verify
	// which tier they were actually evaluated against.
	UserType string `json:"user_type"`
}

// AdminUsageResponse is the paginated admin response.
type AdminUsageResponse struct {
	Total     int64           `json:"total"`
	Limit     int64           `json:"limit"`
	Offset    int64           `json:"offset"`
	Consumers []ConsumerUsage `json:"consumers"`
}
