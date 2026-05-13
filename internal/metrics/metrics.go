package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// RequestsTotal counts all completed requests labelled by mode (async/sync),
	// service_type, model, and HTTP status code.
	RequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "kevent_requests_total",
		Help: "Total number of requests handled by the gateway.",
	}, []string{"mode", "service_type", "model", "status"})

	// RequestDuration measures end-to-end handler latency.
	RequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "kevent_request_duration_seconds",
		Help:    "End-to-end request duration in seconds.",
		Buckets: []float64{.1, .5, 1, 5, 10, 30, 60, 120, 300},
	}, []string{"mode", "service_type", "model"})

	// SyncWaitDuration measures the time the gateway spends blocked on the Redis
	// pub/sub notification in sync-over-Kafka mode.
	SyncWaitDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "kevent_sync_wait_duration_seconds",
		Help:    "Time spent waiting for sync-over-Kafka job results (Redis pub/sub).",
		Buckets: []float64{.5, 1, 5, 10, 30, 60, 120, 300},
	}, []string{"service_type", "model"})

	// SyncJobsInFlight tracks the number of sync-over-Kafka connections that are
	// currently open and waiting for relay results.
	SyncJobsInFlight = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "kevent_sync_jobs_in_flight",
		Help: "Number of sync-over-Kafka jobs currently waiting for results.",
	})

	// S3OperationDuration measures latency for each S3 operation (upload/get/delete).
	S3OperationDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "kevent_s3_operation_duration_seconds",
		Help:    "S3 operation duration in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{"operation"})

	// S3ErrorsTotal counts S3 operation failures.
	S3ErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "kevent_s3_errors_total",
		Help: "Total number of S3 operation errors.",
	}, []string{"operation"})

	// KafkaPublishDuration measures Kafka WriteMessages latency per topic.
	KafkaPublishDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "kevent_kafka_publish_duration_seconds",
		Help:    "Kafka publish duration in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{"topic"})

	// KafkaPublishErrorsTotal counts Kafka publish failures per topic.
	KafkaPublishErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "kevent_kafka_publish_errors_total",
		Help: "Total number of Kafka publish errors.",
	}, []string{"topic"})

	// RedisOperationDuration measures latency for each Redis operation.
	RedisOperationDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "kevent_redis_operation_duration_seconds",
		Help:    "Redis operation duration in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{"operation"})

	// RedisErrorsTotal counts Redis operation failures.
	RedisErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "kevent_redis_errors_total",
		Help: "Total number of Redis operation errors.",
	}, []string{"operation"})

	// JobsByConsumerTotal counts submitted jobs per consumer, labelled by
	// service_type, model, and consumer name (from the configurable consumer header).
	// Only incremented when consumer_header is configured and the header is present.
	JobsByConsumerTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "kevent_jobs_by_consumer_total",
		Help: "Total number of jobs submitted per consumer.",
	}, []string{"mode", "service_type", "model", "consumer"})

	// RateLimitRequestsTotal counts rate-limit evaluations, labelled by
	// service_type, user_type (from the configurable user_type_header), and result
	// ("allowed" or "rejected"). Consumer name is intentionally omitted to keep
	// cardinality low; use RateLimitConsumerHitsTotal for per-consumer analysis.
	// Only populated when rate_limits is configured.
	RateLimitRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "kevent_ratelimit_requests_total",
		Help: "Total number of requests evaluated by the rate limiter, by outcome.",
	}, []string{"service_type", "user_type", "result"})

	// RateLimitConsumerHitsTotal counts rate-limit evaluations per consumer,
	// enabling `count by (user_type) (group by (...) (...))` in PromQL to get
	// the number of distinct consumers per user_type.
	// Only populated when both rate_limits and consumer_header are configured.
	RateLimitConsumerHitsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "kevent_ratelimit_consumer_hits_total",
		Help: "Total rate-limit evaluations per consumer (enables distinct consumer count per user_type via PromQL group).",
	}, []string{"service_type", "user_type", "consumer"})

	// RateLimitErrorsTotal counts Redis errors during rate-limit evaluation (fail-open).
	RateLimitErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "kevent_ratelimit_errors_total",
		Help: "Total number of Redis errors during rate-limit checks (requests are allowed on error).",
	}, []string{"service_type"})

	// LLM proxy + cache metrics
	CacheHitsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "kevent_cache_hits_total",
		Help: "LLM response cache hits.",
	}, []string{"service_type", "model"})

	CacheMissesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "kevent_cache_misses_total",
		Help: "LLM response cache misses.",
	}, []string{"service_type", "model"})

	CacheErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "kevent_cache_errors_total",
		Help: "LLM response cache errors.",
	}, []string{"service_type", "model", "operation"}) // operation: get|set|key

	// user_type label = "sa" | "user" | "" (when UserTypeHeader not configured).
	LLMTokensTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "kevent_llm_tokens_total",
		Help: "Tokens served by LLM requests (prompt+completion, includes cache hits).",
	}, []string{"service_type", "model", "backend_model", "user_type", "type"})

	LLMRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "kevent_llm_requests_total",
		Help: "Total LLM requests by provider, user_type, and HTTP status.",
	}, []string{"service_type", "model", "backend_model", "provider", "user_type", "status"})

	LLMRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "kevent_llm_request_duration_seconds",
		Help:    "End-to-end LLM request latency.",
		Buckets: []float64{.05, .1, .25, .5, 1, 2, 5, 10, 30, 60, 120},
	}, []string{"service_type", "model", "backend_model", "provider", "user_type"})

	// LLMTokensPerRequest is a histogram of tokens per request, enabling p50/p95/p99
	// analysis by user_type. Useful to detect large contexts and capacity planning.
	LLMTokensPerRequest = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "kevent_llm_tokens_per_request",
		Help:    "Distribution of total tokens (prompt+completion) per LLM request.",
		Buckets: []float64{50, 100, 250, 500, 1000, 2000, 5000, 10000, 32000, 100000},
	}, []string{"service_type", "model", "backend_model", "user_type"})

	// LLMConsumerTokensTop exposes the top-N consumers by token usage, refreshed
	// periodically from a Redis sorted set. Only populated when metrics.top_consumers > 0.
	LLMConsumerTokensTop = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kevent_llm_consumer_tokens_top",
		Help: "Token usage for top consumers (refreshed from Redis sorted set).",
	}, []string{"consumer", "user_type", "type"})

	// BackendEffectiveWeight tracks the effective routing weight for each LLM
	// backend after health-penalty adjustment. A value of 0 means the backend
	// is excluded from primary selection due to a high error rate.
	BackendEffectiveWeight = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kevent_backend_effective_weight",
		Help: "Effective routing weight for each LLM backend after health adjustment (0 = excluded).",
	}, []string{"url"})
)
