// Package ui implements the read-only GatewAI dashboard: self-service quota
// view for each consumer and an admin view for privileged users.
package ui

import (
	"context"
	"html/template"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"gatewai/gateway/internal/auth"
	"gatewai/gateway/internal/config"
	"gatewai/gateway/internal/pgstore"
)

func (h *Handler) base(activePage string, consumer, userType string, isAdmin bool) basePage {
	return basePage{
		ActivePage: activePage,
		Consumer:   consumer,
		UserType:   userType,
		IsAdmin:    isAdmin,
		BasePath:   h.basePath,
	}
}

// Handler serves all UI routes.
type Handler struct {
	store       *pgstore.Store // nil when Postgres is not configured
	redis       RedisReader
	tmpl        *template.Template
	adminGroups []string
	adminRoles  []string
	rateLimits  map[string]map[string]config.RateLimitConfig
	basePath    string // URL prefix, e.g. "/ui" or "" for root
}

// New creates a Handler. store may be nil (Postgres disabled); redis is required.
// basePath is the URL prefix under which the UI is mounted (e.g. "/ui" or "").
func New(
	store *pgstore.Store,
	redis RedisReader,
	adminGroups, adminRoles []string,
	rateLimits map[string]map[string]config.RateLimitConfig,
	basePath string,
) (*Handler, error) {
	tmpl, err := parseTemplates()
	if err != nil {
		return nil, err
	}
	return &Handler{
		store:       store,
		redis:       redis,
		tmpl:        tmpl,
		adminGroups: adminGroups,
		adminRoles:  adminRoles,
		rateLimits:  rateLimits,
		basePath:    strings.TrimRight(basePath, "/"),
	}, nil
}

func (h *Handler) isAdmin(p *auth.Principal) bool {
	if p == nil {
		return false
	}
	for _, g := range p.Groups {
		for _, ag := range h.adminGroups {
			if g == ag {
				return true
			}
		}
	}
	for _, r := range p.Roles {
		for _, ar := range h.adminRoles {
			if r == ar {
				return true
			}
		}
	}
	return false
}

func (h *Handler) principalOrEmpty(r *http.Request) (*auth.Principal, string, string, bool) {
	p, ok := auth.FromContext(r.Context())
	if !ok || p == nil {
		return nil, "", "", false
	}
	return p, p.Consumer, p.UserType, h.isAdmin(p)
}

// Health handles GET /healthz — liveness probe, no auth required.
func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// Dashboard handles GET / — self-service quota + 30-day summary.
func (h *Handler) Dashboard(w http.ResponseWriter, r *http.Request) {
	p, consumer, userType, isAdmin := h.principalOrEmpty(r)
	if !authenticated(p) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	ctx := r.Context()
	data := DashboardData{
		basePage:   h.base("dashboard", consumer, userType, isAdmin),
		HasHistory: h.store != nil,
	}

	data.Quotas = h.buildQuotaRows(r.Context(), consumer, userType)

	if h.store != nil {
		summary, err := h.store.GetUsageSummary(ctx, consumer, time.Now().UTC().Add(-30*24*time.Hour))
		if err != nil {
			slog.WarnContext(ctx, "ui: get usage summary", "error", err)
		} else {
			data.Summary = summary
		}
	}

	h.render(w, "dashboard.html", data)
}

// QuotaPartial handles GET /partials/quota — HTMX-polled quota cards fragment.
func (h *Handler) QuotaPartial(w http.ResponseWriter, r *http.Request) {
	p, consumer, userType, isAdmin := h.principalOrEmpty(r)
	if !authenticated(p) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	data := DashboardData{
		basePage: h.base("", consumer, userType, isAdmin),
	}
	data.Quotas = h.buildQuotaRows(r.Context(), consumer, userType)
	if err := h.tmpl.ExecuteTemplate(w, "quota_cards", data); err != nil {
		slog.WarnContext(r.Context(), "ui: render quota_cards", "error", err)
	}
}

// History handles GET /history — paginated event table.
func (h *Handler) History(w http.ResponseWriter, r *http.Request) {
	p, consumer, userType, isAdmin := h.principalOrEmpty(r)
	if !authenticated(p) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	if h.store == nil {
		h.render(w, "history.html", HistoryData{
			basePage: h.base("history", consumer, userType, isAdmin),
		})
		return
	}

	ctx := r.Context()
	q := r.URL.Query()

	days, _ := strconv.Atoi(q.Get("days"))
	if days <= 0 {
		days = 30
	}
	page, _ := strconv.Atoi(q.Get("page"))
	if page <= 0 {
		page = 1
	}
	const pageSize = 50
	offset := (page - 1) * pageSize

	params := pgstore.QueryParams{
		Consumer:    consumer,
		ServiceType: q.Get("service_type"),
		Since:       time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour),
		Limit:       pageSize,
		Offset:      offset,
	}

	events, total, err := h.store.ListRecentEvents(ctx, params)
	if err != nil {
		slog.WarnContext(ctx, "ui: list recent events", "error", err)
	}

	serviceTypes := h.serviceTypeList()
	data := HistoryData{
		basePage:     h.base("history", consumer, userType, isAdmin),
		Events:       events,
		Total:        total,
		Page:         page,
		Limit:        pageSize,
		Offset:       offset,
		Filters:      HistoryFilters{ServiceType: q.Get("service_type"), Days: days},
		ServiceTypes: serviceTypes,
	}

	if q.Get("partial") == "1" {
		if err := h.tmpl.ExecuteTemplate(w, "history_rows", data); err != nil {
			slog.WarnContext(ctx, "ui: render history_rows", "error", err)
		}
		return
	}
	h.render(w, "history.html", data)
}

// Admin handles GET /admin — admin-only all-consumers view.
func (h *Handler) Admin(w http.ResponseWriter, r *http.Request) {
	p, consumer, userType, isAdmin := h.principalOrEmpty(r)
	if !authenticated(p) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if !isAdmin {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	ctx := r.Context()
	data := AdminData{
		basePage:    h.base("admin", consumer, userType, isAdmin),
		Since:       time.Now().UTC().Add(-30 * 24 * time.Hour),
		QueueDepths: map[string]int64{},
	}

	if h.store != nil {
		consumers, err := h.store.ListConsumerSummaries(ctx, data.Since)
		if err != nil {
			slog.WarnContext(ctx, "ui: list consumer summaries", "error", err)
		} else {
			data.Consumers = consumers
		}
	}

	h.render(w, "admin.html", data)
}

// AdminConsumer handles GET /admin/consumer/{name}.
func (h *Handler) AdminConsumer(w http.ResponseWriter, r *http.Request) {
	p, consumer, userType, isAdmin := h.principalOrEmpty(r)
	if !authenticated(p) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if !isAdmin {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	targetConsumer := chi.URLParam(r, "name")
	ctx := r.Context()

	data := AdminConsumerData{
		basePage:     h.base("admin", consumer, userType, isAdmin),
		ConsumerName: targetConsumer,
	}

	if h.store != nil {
		summaries, err := h.store.ListConsumerSummaries(ctx, time.Now().UTC().Add(-30*24*time.Hour))
		if err == nil {
			for _, s := range summaries {
				if s.Consumer == targetConsumer {
					data.Summary = s
					break
				}
			}
		}

		events, _, err := h.store.ListRecentEvents(ctx, pgstore.QueryParams{
			Consumer: targetConsumer,
			Limit:    100,
			Since:    time.Now().UTC().Add(-30 * 24 * time.Hour),
		})
		if err != nil {
			slog.WarnContext(ctx, "ui: list events for consumer", "consumer", targetConsumer, "error", err)
		} else {
			data.Events = events
		}
	}

	h.render(w, "admin_consumer.html", data)
}

func (h *Handler) buildQuotaRows(ctx context.Context, consumer, userType string) []QuotaRow {
	if h.redis == nil {
		return nil
	}

	var rows []QuotaRow
	seen := map[string]bool{}
	for svcType, userLimits := range h.rateLimits {
		for ut, lim := range userLimits {
			key := svcType + ":" + ut
			if seen[key] {
				continue
			}
			seen[key] = true

			// Only show quotas matching the user's own user_type or the "*" fallback.
			if ut != userType && ut != "*" {
				continue
			}

			requests, tokens, ttl, err := h.redis.CurrentUsage(ctx, consumer, svcType, ut)
			if err != nil {
				continue
			}
			rows = append(rows, QuotaRow{
				ServiceType:  svcType,
				UserType:     ut,
				Requests:     requests,
				RequestLimit: int64(lim.Rate),
				Tokens:       tokens,
				TokenLimit:   int64(lim.TokenRate),
				TTLSeconds:   ttl,
			})
		}
	}
	return rows
}

func (h *Handler) serviceTypeList() []string {
	seen := map[string]bool{}
	var types []string
	for st := range h.rateLimits {
		if !seen[st] {
			seen[st] = true
			types = append(types, st)
		}
	}
	return types
}

func (h *Handler) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.tmpl.ExecuteTemplate(w, name, data); err != nil {
		slog.Warn("ui: render template", "template", name, "error", err)
		if !strings.Contains(err.Error(), "write:") {
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
	}
}

func authenticated(p *auth.Principal) bool {
	return p != nil && p.Authenticated
}
