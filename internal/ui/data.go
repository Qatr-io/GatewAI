package ui

import (
	"time"

	"gatewai/gateway/internal/pgstore"
)

// basePage fields embedded in every page data struct.
type basePage struct {
	ActivePage string
	Consumer   string
	UserType   string
	IsAdmin    bool
	BasePath   string // URL prefix, e.g. "/ui" or "" for root
}

// QuotaRow represents one service/user-type quota card.
type QuotaRow struct {
	ServiceType  string
	UserType     string
	Requests     int64
	RequestLimit int64
	Tokens       int64
	TokenLimit   int64
	TTLSeconds   int64
}

// TTLMinutes returns TTL rounded to minutes for display.
func (q QuotaRow) TTLMinutes() int64 {
	if q.TTLSeconds <= 0 {
		return 0
	}
	return (q.TTLSeconds + 59) / 60
}

// RequestPct returns the percentage of request quota consumed (0–100).
func (q QuotaRow) RequestPct() int {
	if q.RequestLimit <= 0 {
		return 0
	}
	pct := int(q.Requests * 100 / q.RequestLimit)
	if pct > 100 {
		pct = 100
	}
	return pct
}

// BarClass returns a CSS class for the progress bar colour.
func (q QuotaRow) BarClass() string {
	pct := q.RequestPct()
	switch {
	case pct >= 90:
		return "crit"
	case pct >= 70:
		return "warn"
	default:
		return ""
	}
}

// DashboardData is the template data for GET /.
type DashboardData struct {
	basePage
	Quotas     []QuotaRow
	Summary    []pgstore.UsageSummary
	HasHistory bool // false when Postgres is not configured
}

// HistoryFilters holds the active filter values from the query string.
type HistoryFilters struct {
	ServiceType string
	Days        int
}

// HistoryData is the template data for GET /history.
type HistoryData struct {
	basePage
	Events       []pgstore.RecentEvent
	Total        int64
	Page         int
	Limit        int
	Offset       int
	Filters      HistoryFilters
	ServiceTypes []string // for the filter dropdown
}

// AdminData is the template data for GET /admin.
type AdminData struct {
	basePage
	Consumers   []pgstore.ConsumerSummary
	QueueDepths map[string]int64
	Since       time.Time
}

// AdminConsumerData is the template data for GET /admin/consumer/{name}.
type AdminConsumerData struct {
	basePage
	ConsumerName string
	Summary      pgstore.ConsumerSummary
	Events       []pgstore.RecentEvent
}
