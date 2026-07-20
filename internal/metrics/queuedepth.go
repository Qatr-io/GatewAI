package metrics

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"gatewai/gateway/internal/service"
)

// relayQueueStates are the Redis list suffixes a job passes through:
// relay:{model}:pending -> relay:{model}:processing (see internal/storage.RedisClient).
var relayQueueStates = [2]string{"pending", "processing"}

// RelayQueueDepthCollector reports the live length of each async model's relay
// queues as gatewai_relay_queue_depth{model,state}. It exists so the "Queue
// relay — Profondeur par modèle" Grafana panel works without deploying
// redis_exporter: the gateway already knows the queue key names, so it can
// expose their depth itself.
//
// Depths are read from Redis on every /metrics scrape (LLEN is O(1)) rather
// than cached by a background goroutine, so the value is always current and
// there's no extra lifecycle to manage.
type RelayQueueDepthCollector struct {
	redis *redis.Client
	reg   atomic.Pointer[service.Registry]
	desc  *prometheus.Desc
}

// NewRelayQueueDepthCollector creates a collector reading queue depths via redisClient.
func NewRelayQueueDepthCollector(redisClient *redis.Client, reg *service.Registry) *RelayQueueDepthCollector {
	c := &RelayQueueDepthCollector{
		redis: redisClient,
		desc: prometheus.NewDesc(
			"gatewai_relay_queue_depth",
			"Number of jobs in a relay queue, labelled by model and state (pending|processing).",
			[]string{"model", "state"}, nil,
		),
	}
	c.reg.Store(reg)
	return c
}

// UpdateRegistry swaps the registry used to enumerate async models (hot-reload).
func (c *RelayQueueDepthCollector) UpdateRegistry(reg *service.Registry) {
	c.reg.Store(reg)
}

func (c *RelayQueueDepthCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.desc
}

func (c *RelayQueueDepthCollector) Collect(ch chan<- prometheus.Metric) {
	reg := c.reg.Load()
	if reg == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	seen := make(map[string]struct{})
	for _, def := range reg.All() {
		if !def.SupportsAsync || def.Model == "" {
			continue
		}
		if _, ok := seen[def.Model]; ok {
			continue
		}
		seen[def.Model] = struct{}{}

		for _, state := range relayQueueStates {
			n, err := c.redis.LLen(ctx, "relay:"+def.Model+":"+state).Result()
			if err != nil {
				slog.Warn("relay queue depth: LLEN failed", "model", def.Model, "state", state, "error", err)
				continue
			}
			ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(n), def.Model, state)
		}
	}
}
