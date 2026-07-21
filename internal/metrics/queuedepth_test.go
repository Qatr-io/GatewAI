package metrics_test

import (
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"

	"gatewai/gateway/internal/config"
	gmetrics "gatewai/gateway/internal/metrics"
	"gatewai/gateway/internal/service"
)

func newTestRegistry() *service.Registry {
	return service.NewRegistry([]config.ServiceConfig{{
		Type:          "transcription",
		Model:         "whisper-large-v3",
		AcceptedExts:  []string{".mp3"},
		MaxFileSizeMB: 100,
	}})
}

func TestRelayQueueDepthCollector_ReportsListLengths(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	mr.Push("relay:whisper-large-v3:pending", "job-1", "job-2")
	mr.Push("relay:whisper-large-v3:processing", "job-3")

	c := gmetrics.NewRelayQueueDepthCollector(rdb, newTestRegistry())

	expected := `
# HELP gatewai_relay_queue_depth Number of jobs in a relay queue, labelled by model and state (pending|processing).
# TYPE gatewai_relay_queue_depth gauge
gatewai_relay_queue_depth{model="whisper-large-v3",state="pending"} 2
gatewai_relay_queue_depth{model="whisper-large-v3",state="processing"} 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected), "gatewai_relay_queue_depth"); err != nil {
		t.Errorf("unexpected collector output: %v", err)
	}
}

func TestRelayQueueDepthCollector_SkipsSyncOnlyModels(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	reg := service.NewRegistry([]config.ServiceConfig{{
		Type:         "llm",
		Model:        "gpt-4o",
		InferenceURL: "http://inference.svc.cluster.local",
		Provider:     "openai",
	}})

	c := gmetrics.NewRelayQueueDepthCollector(rdb, reg)

	if err := testutil.CollectAndCompare(c, strings.NewReader(""), "gatewai_relay_queue_depth"); err != nil {
		t.Errorf("expected no metrics for a sync-only model, got: %v", err)
	}
}

func TestRelayQueueDepthCollector_UpdateRegistry(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	mr.Push("relay:new-model:pending", "job-1")

	c := gmetrics.NewRelayQueueDepthCollector(rdb, service.NewRegistry(nil))

	newReg := service.NewRegistry([]config.ServiceConfig{{
		Type:          "transcription",
		Model:         "new-model",
		AcceptedExts:  []string{".mp3"},
		MaxFileSizeMB: 100,
	}})
	c.UpdateRegistry(newReg)

	expected := `
# HELP gatewai_relay_queue_depth Number of jobs in a relay queue, labelled by model and state (pending|processing).
# TYPE gatewai_relay_queue_depth gauge
gatewai_relay_queue_depth{model="new-model",state="pending"} 1
gatewai_relay_queue_depth{model="new-model",state="processing"} 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected), "gatewai_relay_queue_depth"); err != nil {
		t.Errorf("unexpected collector output after UpdateRegistry: %v", err)
	}
}
