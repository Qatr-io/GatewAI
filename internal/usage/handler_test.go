package usage_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gatewai/gateway/internal/config"
	"gatewai/gateway/internal/usage"
)

// fakeStore is a test double for UsageStore.
type fakeStore struct {
	consumerUsage map[string]*usage.ConsumerUsage
	consumers     []string
	total         int64

	report    *usage.UsageReport
	reportErr error
}

func (f *fakeStore) GetConsumerUsage(_ context.Context, consumer, _ string, _ []string, _ map[string][]string) (*usage.ConsumerUsage, error) {
	if cu, ok := f.consumerUsage[consumer]; ok {
		return cu, nil
	}
	return &usage.ConsumerUsage{Consumer: consumer, Retention: "all-time", Usage: nil}, nil
}

func (f *fakeStore) ListConsumers(_ context.Context, _, _ int64) ([]string, int64, error) {
	return f.consumers, f.total, nil
}

func (f *fakeStore) ListConsumersByType(_ context.Context, _ string, _, _ int64) ([]string, int64, error) {
	return f.consumers, f.total, nil
}

func (f *fakeStore) UpdateRateLimits(_, _ map[string]map[string]config.RateLimitConfig) {}

func (f *fakeStore) GetUsageReport(_ context.Context, serviceType, period string, from, to time.Time) (*usage.UsageReport, error) {
	if f.reportErr != nil {
		return nil, f.reportErr
	}
	if f.report != nil {
		return f.report, nil
	}
	return &usage.UsageReport{
		ServiceType: serviceType,
		Period:      period,
		From:        from.Format("2006-01-02"),
		To:          to.Format("2006-01-02"),
	}, nil
}

func newHandlerFromStore(store usage.UsageStore) *usage.UsageHandler {
	return usage.NewUsageHandler(store, nil, "X-Consumer", "X-User-Type")
}

func TestGetMyUsage_MissingConsumerHeader_400(t *testing.T) {
	h := newHandlerFromStore(&fakeStore{})
	r := httptest.NewRequest("GET", "/usage", nil)
	w := httptest.NewRecorder()
	h.GetMyUsage(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("got %d, want 400", w.Code)
	}
}

func TestGetMyUsage_NoConsumerHeaderConfigured_501(t *testing.T) {
	h := usage.NewUsageHandler(&fakeStore{}, nil, "", "")
	r := httptest.NewRequest("GET", "/usage", nil)
	w := httptest.NewRecorder()
	h.GetMyUsage(w, r)
	if w.Code != http.StatusNotImplemented {
		t.Errorf("got %d, want 501", w.Code)
	}
}

func TestGetMyUsage_ReturnsJSON(t *testing.T) {
	store := &fakeStore{
		consumerUsage: map[string]*usage.ConsumerUsage{
			"alice": {
				Consumer:  "alice",
				Retention: "all-time",
				Usage: []usage.ServiceUsage{
					{ServiceType: "audio", Total: usage.TotalUsage{Requests: 10}},
				},
			},
		},
	}
	h := newHandlerFromStore(store)
	r := httptest.NewRequest("GET", "/usage", nil)
	r.Header.Set("X-Consumer", "alice")
	w := httptest.NewRecorder()
	h.GetMyUsage(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("got %d, want 200", w.Code)
	}
	var resp usage.ConsumerUsage
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Consumer != "alice" {
		t.Errorf("consumer = %q", resp.Consumer)
	}
	if len(resp.Usage) != 1 || resp.Usage[0].Total.Requests != 10 {
		t.Errorf("unexpected usage: %+v", resp.Usage)
	}
}

func TestGetMyUsage_ContentTypeJSON(t *testing.T) {
	h := newHandlerFromStore(&fakeStore{})
	r := httptest.NewRequest("GET", "/usage", nil)
	r.Header.Set("X-Consumer", "alice")
	w := httptest.NewRecorder()
	h.GetMyUsage(w, r)
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

func TestAdminListUsage_DefaultPagination(t *testing.T) {
	store := &fakeStore{
		consumers: []string{"alice", "bob"},
		total:     2,
		consumerUsage: map[string]*usage.ConsumerUsage{
			"alice": {Consumer: "alice", Retention: "all-time"},
			"bob":   {Consumer: "bob", Retention: "all-time"},
		},
	}
	h := newHandlerFromStore(store)
	r := httptest.NewRequest("GET", "/-/usage", nil)
	w := httptest.NewRecorder()
	h.AdminListUsage(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("got %d, want 200", w.Code)
	}
	var resp usage.AdminUsageResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Total != 2 {
		t.Errorf("total = %d, want 2", resp.Total)
	}
	if resp.Limit != 20 {
		t.Errorf("limit = %d, want 20", resp.Limit)
	}
	if resp.Offset != 0 {
		t.Errorf("offset = %d, want 0", resp.Offset)
	}
	if len(resp.Consumers) != 2 {
		t.Errorf("consumers len = %d, want 2", len(resp.Consumers))
	}
}

func TestAdminListUsage_FilterByConsumer(t *testing.T) {
	store := &fakeStore{
		consumerUsage: map[string]*usage.ConsumerUsage{
			"alice": {Consumer: "alice", Retention: "all-time"},
		},
	}
	h := newHandlerFromStore(store)
	r := httptest.NewRequest("GET", "/-/usage?consumer=alice", nil)
	w := httptest.NewRecorder()
	h.AdminListUsage(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("got %d, want 200", w.Code)
	}
	var resp usage.AdminUsageResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Total != 1 || len(resp.Consumers) != 1 {
		t.Errorf("expected single consumer: %+v", resp)
	}
	if resp.Consumers[0].Consumer != "alice" {
		t.Errorf("consumer = %q", resp.Consumers[0].Consumer)
	}
}

func TestAdminListUsage_LimitCappedAt100(t *testing.T) {
	h := newHandlerFromStore(&fakeStore{consumers: []string{}, total: 0})
	r := httptest.NewRequest("GET", "/-/usage?limit=500", nil)
	w := httptest.NewRecorder()
	h.AdminListUsage(w, r)

	var resp usage.AdminUsageResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Limit != 100 {
		t.Errorf("limit should be capped at 100, got %d", resp.Limit)
	}
}

func TestAdminListUsage_Offset(t *testing.T) {
	h := newHandlerFromStore(&fakeStore{consumers: []string{}, total: 50})
	r := httptest.NewRequest("GET", "/-/usage?offset=10", nil)
	w := httptest.NewRecorder()
	h.AdminListUsage(w, r)

	var resp usage.AdminUsageResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Offset != 10 {
		t.Errorf("offset = %d, want 10", resp.Offset)
	}
}
