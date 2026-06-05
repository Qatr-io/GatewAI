package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// RequestsTotal counts all completed requests labelled by mode (async/sync),
	// service_type, model, and HTTP status code.
	RequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_requests_total",
		Help: "Total number of requests handled by the gateway.",
	}, []string{"mode", "service_type", "model", "status"})

	// RequestDuration measures end-to-end handler latency.
	RequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "gatewai_request_duration_seconds",
		Help:    "End-to-end request duration in seconds.",
		Buckets: []float64{.1, .5, 1, 5, 10, 30, 60, 120, 300},
	}, []string{"mode", "service_type", "model"})

	// S3OperationDuration measures latency for each S3 operation (upload/get/delete).
	S3OperationDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "gatewai_s3_operation_duration_seconds",
		Help:    "S3 operation duration in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{"operation"})

	// S3ErrorsTotal counts S3 operation failures.
	S3ErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_s3_errors_total",
		Help: "Total number of S3 operation errors.",
	}, []string{"operation"})

	// RedisOperationDuration measures latency for each Redis operation.
	RedisOperationDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "gatewai_redis_operation_duration_seconds",
		Help:    "Redis operation duration in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{"operation"})

	// RedisErrorsTotal counts Redis operation failures.
	RedisErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_redis_errors_total",
		Help: "Total number of Redis operation errors.",
	}, []string{"operation"})

	// JobsByConsumerTotal counts submitted jobs per consumer, labelled by
	// service_type, model, and consumer name (from the configurable consumer header).
	// Only incremented when consumer_header is configured and the header is present.
	JobsByConsumerTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_jobs_by_consumer_total",
		Help: "Total number of jobs submitted per consumer.",
	}, []string{"mode", "service_type", "model", "consumer"})

	// RateLimitRequestsTotal counts rate-limit evaluations, labelled by
	// service_type, user_type (from the configurable user_type_header), and result
	// ("allowed" or "rejected"). Consumer name is intentionally omitted to keep
	// cardinality low; use RateLimitConsumerHitsTotal for per-consumer analysis.
	// Only populated when rate_limits is configured.
	RateLimitRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_ratelimit_requests_total",
		Help: "Total number of requests evaluated by the rate limiter, by outcome.",
	}, []string{"service_type", "user_type", "result"})

	// RateLimitConsumerHitsTotal counts rate-limit evaluations per consumer,
	// enabling `count by (user_type) (group by (...) (...))` in PromQL to get
	// the number of distinct consumers per user_type.
	// Only populated when both rate_limits and consumer_header are configured.
	RateLimitConsumerHitsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_ratelimit_consumer_hits_total",
		Help: "Total rate-limit evaluations per consumer (enables distinct consumer count per user_type via PromQL group).",
	}, []string{"service_type", "user_type", "consumer"})

	// RateLimitErrorsTotal counts Redis errors during rate-limit evaluation (fail-open).
	RateLimitErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_ratelimit_errors_total",
		Help: "Total number of Redis errors during rate-limit checks (requests are allowed on error).",
	}, []string{"service_type"})

	// LLM proxy + cache metrics
	CacheHitsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_cache_hits_total",
		Help: "LLM response cache hits.",
	}, []string{"service_type", "model"})

	CacheMissesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_cache_misses_total",
		Help: "LLM response cache misses.",
	}, []string{"service_type", "model"})

	CacheErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_cache_errors_total",
		Help: "LLM response cache errors.",
	}, []string{"service_type", "model", "operation"}) // operation: get|set|key

	// user_type label = "sa" | "user" | "" (when UserTypeHeader not configured).
	LLMTokensTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_llm_tokens_total",
		Help: "Tokens served by LLM requests (prompt+completion, includes cache hits).",
	}, []string{"service_type", "model", "backend_model", "user_type", "type"})

	LLMRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_llm_requests_total",
		Help: "Total LLM requests by provider, user_type, and HTTP status.",
	}, []string{"service_type", "model", "backend_model", "provider", "user_type", "status"})

	LLMRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "gatewai_llm_request_duration_seconds",
		Help:    "End-to-end LLM request latency.",
		Buckets: []float64{.05, .1, .25, .5, 1, 2, 5, 10, 30, 60, 120},
	}, []string{"service_type", "model", "backend_model", "provider", "user_type"})

	// LLMTokensPerRequest is a histogram of tokens per request, enabling p50/p95/p99
	// analysis by user_type. Useful to detect large contexts and capacity planning.
	LLMTokensPerRequest = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "gatewai_llm_tokens_per_request",
		Help:    "Distribution of total tokens (prompt+completion) per LLM request.",
		Buckets: []float64{50, 100, 250, 500, 1000, 2000, 5000, 10000, 32000, 100000},
	}, []string{"service_type", "model", "backend_model", "user_type"})

	// LLMConsumerTokensTop exposes the top-N consumers by token usage, refreshed
	// periodically from a Redis sorted set. Only populated when metrics.top_consumers > 0.
	LLMConsumerTokensTop = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "gatewai_llm_consumer_tokens_top",
		Help: "Token usage for top consumers (refreshed from Redis sorted set).",
	}, []string{"consumer", "user_type", "type"})

	// GuardrailsPiiBlockedTotal counts requests rejected by the PII guardrail,
	// before they reach the LLM backend.
	GuardrailsPiiBlockedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_guardrails_pii_blocked_total",
		Help: "Total requests blocked by the PII guardrail.",
	}, []string{"service_type", "model"})

	AsyncStaleJobsSweptTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_async_stale_jobs_swept_total",
		Help: "Total number of pending jobs marked failed and cleaned up by the stale-job GC.",
	}, []string{"model"})

	AsyncJobsSubmittedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_async_jobs_submitted_total",
		Help: "Total async jobs accepted (202) by service type and model.",
	}, []string{"service_type", "model"})

	AsyncJobsCancelledTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_async_jobs_cancelled_total",
		Help: "Total async jobs cancelled by the client (DELETE /jobs/{type}/{id}).",
	}, []string{"service_type", "model"})

	AsyncJobsCancelledWhileProcessingTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_async_jobs_cancelled_while_processing_total",
		Help: "Total async jobs cancelled while the relay was processing them (GPU interrupted).",
	}, []string{"service_type", "model"})

	AsyncJobsPurgedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_async_jobs_purged_total",
		Help: "Total async jobs deleted by the admin purge endpoint.",
	}, []string{"model"})
)
