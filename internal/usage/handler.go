package usage

import (
	"encoding/json"
	"net/http"
	"strconv"

	"gatewai/gateway/internal/service"
)

// UsageHandler serves GET /usage (consumer) and GET /-/usage (admin).
type UsageHandler struct {
	store          UsageStore
	reg            *service.Registry // may be nil in tests
	consumerHeader string
	userTypeHeader string
}

// NewUsageHandler returns a configured UsageHandler.
func NewUsageHandler(store UsageStore, reg *service.Registry, consumerHeader, userTypeHeader string) *UsageHandler {
	return &UsageHandler{
		store:          store,
		reg:            reg,
		consumerHeader: consumerHeader,
		userTypeHeader: userTypeHeader,
	}
}

// UpdateRegistry swaps the service registry atomically (called on hot-reload).
func (h *UsageHandler) UpdateRegistry(reg *service.Registry) {
	h.reg = reg
}

// GetMyUsage handles GET /usage.
// Requires consumer_header to be configured; returns 501 otherwise.
// Returns 400 if the header is missing from the request.
func (h *UsageHandler) GetMyUsage(w http.ResponseWriter, r *http.Request) {
	if h.consumerHeader == "" {
		writeJSONError(w, http.StatusNotImplemented, "GET /usage requires consumer_header to be configured")
		return
	}
	consumer := r.Header.Get(h.consumerHeader)
	if consumer == "" {
		writeJSONError(w, http.StatusBadRequest, "missing consumer header: "+h.consumerHeader)
		return
	}
	userType := "*"
	if h.userTypeHeader != "" {
		if v := r.Header.Get(h.userTypeHeader); v != "" {
			userType = v
		}
	}

	result, err := h.store.GetConsumerUsage(r.Context(), consumer, userType, h.serviceTypes())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to retrieve usage")
		return
	}

	writeJSON(w, http.StatusOK, result)
}

// AdminListUsage handles GET /-/usage.
// Query params: consumer, type, limit (default 20, max 100), offset (default 0).
func (h *UsageHandler) AdminListUsage(w http.ResponseWriter, r *http.Request) {
	limit := int64(20)
	offset := int64(0)
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			if n > 100 {
				n = 100
			}
			limit = n
		}
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			offset = n
		}
	}

	filterConsumer := r.URL.Query().Get("consumer")
	filterType := r.URL.Query().Get("type")

	userType := "*"
	if h.userTypeHeader != "" {
		if v := r.Header.Get(h.userTypeHeader); v != "" {
			userType = v
		}
	}

	var consumers []string
	var total int64

	switch {
	case filterConsumer != "":
		consumers = []string{filterConsumer}
		total = 1
	case filterType != "":
		var err error
		consumers, total, err = h.store.ListConsumersByType(r.Context(), filterType, limit, offset)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "failed to list consumers")
			return
		}
	default:
		var err error
		consumers, total, err = h.store.ListConsumers(r.Context(), limit, offset)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "failed to list consumers")
			return
		}
	}

	serviceTypes := h.serviceTypes()
	if filterType != "" {
		serviceTypes = []string{filterType}
	}

	result := AdminUsageResponse{
		Total:     total,
		Limit:     limit,
		Offset:    offset,
		Consumers: make([]ConsumerUsage, 0, len(consumers)),
	}
	for _, consumer := range consumers {
		cu, err := h.store.GetConsumerUsage(r.Context(), consumer, userType, serviceTypes)
		if err != nil {
			continue
		}
		result.Consumers = append(result.Consumers, *cu)
	}

	writeJSON(w, http.StatusOK, result)
}

func (h *UsageHandler) serviceTypes() []string {
	if h.reg == nil {
		return nil
	}
	return h.reg.Types()
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeJSONError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
