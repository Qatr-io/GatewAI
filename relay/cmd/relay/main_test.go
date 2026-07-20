package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"

	"gatewai/relay/internal/metrics"
	"gatewai/relay/internal/model"
	"gatewai/relay/internal/queue"
	"gatewai/relay/internal/store"
)

func newTestPublisher(t *testing.T, gatewayBaseURL string) (*redisPublisher, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	return &redisPublisher{
		st:             store.New(rdb),
		q:              queue.New(rdb, "whisper-large-v3"),
		gatewayBaseURL: gatewayBaseURL,
		httpClient:     &http.Client{Timeout: 5 * time.Second},
	}, mr
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

func TestPublishResult_CallbackSuccess_ReturnsNil(t *testing.T) {
	received := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p, mr := newTestPublisher(t, srv.URL)
	seedJob(t, mr, "job-1")

	before := testutil.ToFloat64(metrics.GatewayCallbackErrorsTotal)

	err := p.PublishResult(t.Context(), "job-1", model.JobStatusCompleted, "job-1/result.json", "", 1.5, 10, 5)
	if err != nil {
		t.Fatalf("PublishResult: %v", err)
	}

	select {
	case path := <-received:
		if path != "/-/relay/jobs/job-1/complete" {
			t.Errorf("callback path: got %q", path)
		}
	default:
		t.Error("expected gateway callback to be called")
	}

	if got := testutil.ToFloat64(metrics.GatewayCallbackErrorsTotal); got != before {
		t.Errorf("GatewayCallbackErrorsTotal: got %v, want unchanged %v", got, before)
	}
}

func TestPublishResult_CallbackNon2xx_ReturnsNilButIncrementsMetric(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	p, mr := newTestPublisher(t, srv.URL)
	seedJob(t, mr, "job-2")

	before := testutil.ToFloat64(metrics.GatewayCallbackErrorsTotal)

	err := p.PublishResult(t.Context(), "job-2", model.JobStatusCompleted, "job-2/result.json", "", 1.5, 10, 5)
	if err != nil {
		t.Fatalf("PublishResult: %v", err)
	}

	if got := testutil.ToFloat64(metrics.GatewayCallbackErrorsTotal); got != before+1 {
		t.Errorf("GatewayCallbackErrorsTotal: got %v, want %v", got, before+1)
	}
}

func TestPublishResult_CallbackUnreachable_ReturnsNilButIncrementsMetric(t *testing.T) {
	p, mr := newTestPublisher(t, "http://127.0.0.1:0")
	seedJob(t, mr, "job-3")

	before := testutil.ToFloat64(metrics.GatewayCallbackErrorsTotal)

	err := p.PublishResult(t.Context(), "job-3", model.JobStatusCompleted, "job-3/result.json", "", 1.5, 10, 5)
	if err != nil {
		t.Fatalf("PublishResult: %v", err)
	}

	if got := testutil.ToFloat64(metrics.GatewayCallbackErrorsTotal); got != before+1 {
		t.Errorf("GatewayCallbackErrorsTotal: got %v, want %v", got, before+1)
	}
}
