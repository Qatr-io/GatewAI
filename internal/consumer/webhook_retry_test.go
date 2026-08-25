package consumer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"gatewai/gateway/internal/config"
	"gatewai/gateway/internal/metrics"
	"gatewai/gateway/internal/model"
	"gatewai/gateway/internal/storage"
)

type retryStubS3 struct{}

func (retryStubS3) GetObject(context.Context, string) ([]byte, error) { return nil, nil }
func (retryStubS3) DeleteObject(context.Context, string) error        { return nil }

func newTestSender(t *testing.T, cfg config.WebhookConfig) (*WebhookSender, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rc, err := storage.NewRedis(config.RedisConfig{Addr: mr.Addr()}, config.LifecycleConfig{})
	if err != nil {
		t.Fatalf("redis: %v", err)
	}
	return NewWebhookSender(rc, retryStubS3{}, true, cfg), mr
}

// TestWebhook_FailThenDurableRetrySucceeds verifies that a failed first attempt
// is persisted to the Redis retry queue and delivered on a later pass.
func TestWebhook_FailThenDurableRetrySucceeds(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError) // first attempt fails
			return
		}
		w.WriteHeader(http.StatusOK) // retry succeeds
	}))
	defer srv.Close()

	ws, mr := newTestSender(t, config.WebhookConfig{RetryBackoff: "1ms", MaxBackoff: "10ms", MaxRetries: 3})
	before := testutil.ToFloat64(metrics.WebhookDeliveriesTotal.WithLabelValues("delivered"))

	ws.Send(&model.Job{ID: "j1", ServiceType: "audio", Status: model.JobStatusCompleted, CallbackURL: srv.URL})

	// First attempt failed → one entry queued for retry.
	if n, _ := mr.SortedSet(webhookRetryZSet); len(n) != 1 {
		t.Fatalf("expected 1 queued retry, got %v", n)
	}

	time.Sleep(15 * time.Millisecond) // let the scheduled time pass
	ws.processDueRetries(context.Background())

	if n, _ := mr.SortedSet(webhookRetryZSet); len(n) != 0 {
		t.Fatalf("expected retry queue drained after success, got %v", n)
	}
	if mr.Exists(webhookTaskKey("j1")) {
		t.Fatal("expected task key deleted after delivery")
	}
	if got := testutil.ToFloat64(metrics.WebhookDeliveriesTotal.WithLabelValues("delivered")) - before; got != 1 {
		t.Fatalf("expected 1 delivered increment, got %v", got)
	}
	if calls.Load() != 2 {
		t.Fatalf("expected 2 delivery attempts, got %d", calls.Load())
	}
}

// TestWebhook_DeadLettersAfterMaxRetries verifies a permanently-failing webhook
// lands in the dead-letter list after MaxRetries attempts.
func TestWebhook_DeadLettersAfterMaxRetries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway) // always fails (5xx)
	}))
	defer srv.Close()

	ws, mr := newTestSender(t, config.WebhookConfig{RetryBackoff: "1ms", MaxBackoff: "5ms", MaxRetries: 2})
	before := testutil.ToFloat64(metrics.WebhookDeliveriesTotal.WithLabelValues("deadletter"))

	ws.Send(&model.Job{ID: "j2", ServiceType: "audio", Status: model.JobStatusCompleted, CallbackURL: srv.URL}) // attempt 1

	time.Sleep(10 * time.Millisecond)
	ws.processDueRetries(context.Background()) // attempt 2 → reaches MaxRetries → dead-letter

	items, _ := mr.List(webhookDeadLetter)
	if len(items) != 1 {
		t.Fatalf("expected 1 dead-letter entry, got %v", items)
	}
	if n, _ := mr.SortedSet(webhookRetryZSet); len(n) != 0 {
		t.Fatalf("expected retry queue empty after dead-letter, got %v", n)
	}
	if mr.Exists(webhookTaskKey("j2")) {
		t.Fatal("expected task key deleted after dead-letter")
	}
	if got := testutil.ToFloat64(metrics.WebhookDeliveriesTotal.WithLabelValues("deadletter")) - before; got != 1 {
		t.Fatalf("expected 1 deadletter increment, got %v", got)
	}
}

// TestWebhook_FirstAttemptSuccessNoQueue verifies the happy path never touches
// the retry queue.
func TestWebhook_FirstAttemptSuccessNoQueue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ws, mr := newTestSender(t, config.WebhookConfig{})
	ws.Send(&model.Job{ID: "j3", ServiceType: "audio", Status: model.JobStatusCompleted, CallbackURL: srv.URL})

	if n, _ := mr.SortedSet(webhookRetryZSet); len(n) != 0 {
		t.Fatalf("expected no retry queued on first-attempt success, got %v", n)
	}
}
