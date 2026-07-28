---
title: Metrics reference
---

# Metrics reference

All metrics use the `gatewai_` prefix (gateway) or `gatewai_relay_` prefix (relay). See [Prometheus](../observe/prometheus) for setup instructions.

## Gateway

### Requests

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `gatewai_requests_total` | counter | `mode`, `service_type`, `model`, `status` | Total requests handled by the gateway |
| `gatewai_request_duration_seconds` | histogram | `mode`, `service_type`, `model` | End-to-end request duration. Buckets: 0.1s–300s |

### Async jobs

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `gatewai_async_jobs_submitted_total` | counter | `service_type`, `model` | Jobs accepted (HTTP 202) |
| `gatewai_async_jobs_cancelled_total` | counter | `service_type`, `model` | Jobs cancelled by client (`DELETE /jobs/{type}/{id}`) |
| `gatewai_async_jobs_cancelled_while_processing_total` | counter | `service_type`, `model` | Jobs cancelled while relay was processing them |
| `gatewai_async_jobs_purged_total` | counter | `model` | Jobs deleted via admin purge endpoint |
| `gatewai_async_stale_jobs_swept_total` | counter | `model` | Pending jobs marked failed by the stale-job GC |
| `gatewai_jobs_by_consumer_total` | counter | `mode`, `service_type`, `model`, `consumer` | Jobs submitted per consumer. Requires `metricsConfig.consumerLabels: true` |

### Rate limiting

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `gatewai_ratelimit_requests_total` | counter | `service_type`, `user_type`, `result` | Requests evaluated by rate limiter (`result`: `allowed`\|`rejected`) |
| `gatewai_ratelimit_consumer_hits_total` | counter | `service_type`, `user_type`, `consumer` | Per-consumer rate-limit evaluations (use with `group()` for distinct count) |
| `gatewai_ratelimit_errors_total` | counter | `service_type` | Redis errors during rate-limit checks (requests allowed on error) |
| `gatewai_token_ratelimit_checked_total` | counter | `service_type`, `user_type`, `result` | Token budget checks (`result`: `allowed`\|`rejected`) |
| `gatewai_token_ratelimit_errors_total` | counter | `service_type` | Redis errors during token budget checks |
| `gatewai_concurrent_job_checks_total` | counter | `service_type`, `user_type`, `result` | Concurrent job limit checks |
| `gatewai_processingtime_checks_total` | counter | `service_type`, `user_type`, `result` | Processing time budget checks |

### LLM proxy

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `gatewai_llm_requests_total` | counter | `service_type`, `model`, `backend_model`, `provider`, `user_type`, `status` | LLM requests by provider and HTTP status |
| `gatewai_llm_tokens_total` | counter | `service_type`, `model`, `backend_model`, `user_type`, `type` | Tokens served (`type`: `prompt`\|`completion`), includes cache hits |
| `gatewai_llm_request_duration_seconds` | histogram | `service_type`, `model`, `backend_model`, `provider`, `user_type` | End-to-end LLM request latency. Buckets: 0.05s–120s |
| `gatewai_llm_tokens_per_request` | histogram | `service_type`, `model`, `backend_model`, `user_type` | Total tokens (prompt+completion) per request. Buckets: 50–100 000 |
| `gatewai_llm_consumer_tokens_top` | gauge | `consumer`, `user_type`, `type` | Token usage for top-N consumers (refreshed from Redis every 60s). Requires `metricsConfig.topConsumers > 0` |

### Response cache

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `gatewai_cache_hits_total` | counter | `service_type`, `model` | LLM response cache hits |
| `gatewai_cache_misses_total` | counter | `service_type`, `model` | LLM response cache misses |
| `gatewai_cache_errors_total` | counter | `service_type`, `model`, `operation` | LLM response cache errors |

### Guardrails

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `gatewai_guardrails_pii_blocked_total` | counter | `service_type`, `model` | Requests blocked by PII guardrail |

### Infrastructure

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `gatewai_s3_errors_total` | counter | `operation` | S3 operation errors |
| `gatewai_s3_operation_duration_seconds` | histogram | `operation` | S3 operation duration. Default Prometheus buckets |
| `gatewai_redis_errors_total` | counter | `operation` | Redis operation errors |
| `gatewai_redis_operation_duration_seconds` | histogram | `operation` | Redis operation duration. Default Prometheus buckets |

---

## Relay

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `gatewai_relay_jobs_total` | counter | `service_type`, `status` | Jobs processed by the relay (`status`: `success`\|`failed`) |
| `gatewai_relay_inference_duration_seconds` | histogram | `service_type`, `model` | Inference call duration. Buckets: 0.5s–600s |
| `gatewai_relay_input_size_bytes` | histogram | `service_type` | Input file size. Exponential buckets: 1 KB–256 MB |
| `gatewai_relay_s3_errors_total` | counter | `operation` | Relay S3 operation errors |
| `gatewai_relay_s3_operation_duration_seconds` | histogram | `operation` | Relay S3 operation duration. Default Prometheus buckets |
| `gatewai_relay_redis_publish_errors_total` | counter | — | Redis pub/sub publish errors on job completion |
| `gatewai_relay_redis_done_errors_total` | counter | — | Redis errors when removing job from processing list |
