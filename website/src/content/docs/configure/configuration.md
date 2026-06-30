---
title: Configuration reference
---

# Configuration reference

Configuration is loaded from `config.yaml` (or `$CONFIG_PATH`). Values of the form `${VAR}` or `${VAR:-default}` are expanded from the environment.

## Server

```yaml
server:
  addr: ":8080"
  read_timeout: 120s
  write_timeout: 0s        # 0 = no timeout (recommended for long sync jobs)
  idle_timeout: 120s
  consumer_header: ""      # header name for consumer identification (e.g. X-Consumer-Username)
  priority_header: ""      # header name for priority routing (e.g. X-Priority)
  user_type_header: ""     # header name for user type (e.g. X-User-Type) — rate limiting + LLM metrics
```

### `consumer_header`

When set to a non-empty header name (e.g. `X-Consumer-Username`, typically injected by APISIX after authentication):

- The consumer name is stored in the job record (`consumer_name` field in Redis).
- A Redis sorted set `consumer:{name}:jobs` is maintained per consumer (score = creation timestamp, same TTL as the job). Used by `GET /jobs`.
- `GET /jobs/{service_type}/{id}` enforces **ownership**: if the header is present in the request, `job.consumer_name` must match — returns `404` on mismatch (no information leak about other consumers' jobs).
- `GatewAI_jobs_by_consumer_total{mode, service_type, model, consumer}` is incremented per submission.

Leave empty in deployments without upstream authentication — no behaviour change, zero overhead.

### `priority_header`

When set and a request carries this header, the job is inserted at the **head** of the Redis queue (`LPUSH relay:<model>:pending`) instead of the tail (`RPUSH`). The relay always pops from the head (`BLMOVE LEFT RIGHT`), so priority jobs are picked up first by the next available pod.

Leave empty to disable priority routing.

### `user_type_header`

HTTP header injected by the upstream API gateway (e.g. APISIX, OPA) after token introspection. Typical value: `X-User-Type`. The header value (e.g. `sa`, `user`) is used:

- As a label on all LLM request/token/duration metrics (`user_type`)
- To select the rate limit tier from `rate_limits[service_type][user_type]`

Leave empty to disable user-type differentiation — the `"*"` fallback tier applies for rate limiting.

## Metrics

```yaml
metrics:
  top_consumers: 10        # expose top-N consumers in Prometheus; 0 = disabled
  consumer_labels: false   # direct per-consumer Prometheus labels (< 50 consumers only)
```

### `top_consumers`

When set to a positive integer, enables Redis sorted-set tracking of per-consumer LLM token usage. A background goroutine reads the top-N consumers every 60 seconds (and immediately at startup) and exposes them as `GatewAI_llm_consumer_tokens_top{consumer, user_type, type}`.

Only suitable when `server.consumer_header` is configured. Requires `server.user_type_header` for `user_type` labelling.

:::caution
Do **not** use `consumer_labels: true` with more than ~50 consumers. Each consumer creates a new Prometheus time series; at 100 k+ consumers this causes OOM. Use `top_consumers` instead.
:::

## Rate limits

```yaml
rate_limits:
  audio:
    sa:
      rate: 100
      period: 1m
    user:
      rate: 20
      period: 1m
      token_rate: 500000     # optional: token budget per window
      token_period: 24h
    "*":             # fallback: user_type absent or not listed
      rate: 10
      period: 1m
  ocr:
    "*":
      rate: 5
      period: 1m
```

Per-consumer, per-service fixed-window rate limiting backed by Redis. Returns `429 Too Many Requests` with a `Retry-After` header when exceeded.

| Field | Description |
|---|---|
| Key (e.g. `audio`) | Matches `services[].type` — all models of the same type share the counter |
| Sub-key (e.g. `sa`) | User type from `server.user_type_header`; `"*"` is the catch-all fallback |
| `rate` | Maximum requests allowed in the `period` |
| `period` | Window duration: `30s`, `1m`, `1h`, `24h` |
| `token_rate` | Optional: maximum total LLM tokens per `token_period`; applies on sync LLM requests only |
| `token_period` | Window duration for the token budget: `1h`, `24h` |
| `max_concurrent` | Optional: maximum pending+processing async jobs at the same time per consumer; `0` = disabled |
| `processing_time` | Optional: maximum cumulative inference seconds per `processing_period`; async only; `0` = disabled |
| `processing_period` | Window duration for the processing-time budget: `1h`, `24h` |

Leave `rate_limits` empty to disable. See [Rate limiting](../configure/rate-limiting) for details.

## Token limits per model

For finer-grained control, token budgets can be set **per model** under `services[].token_limits`. This is the recommended approach when several models share the same `type` but should have independent quotas.

```yaml
services:
  - type: llm
    model: gpt-4o
    provider: openai
    inference_url: "https://api.openai.com"
    token_limits:
      user:
        token_rate: 100000
        token_period: 24h
      sa:
        token_rate: 1000000
        token_period: 1h
      "*":
        token_rate: 50000
        token_period: 24h
```

When both `rate_limits[type][user_type].token_rate` and `services[].token_limits[user_type].token_rate` are configured, **both** checks must pass. The model-level limit is enforced first.

## S3

```yaml
s3:
  endpoint: "${S3_ENDPOINT:-https://s3.fr-par.scw.cloud}"
  region: "${S3_REGION:-fr-par}"
  access_key: "${S3_ACCESS_KEY}"
  secret_key: "${S3_SECRET_KEY}"
  bucket: "${S3_BUCKET:-GatewAI-jobs}"
```

## Encryption

```yaml
encryption:
  key: "${ENCRYPTION_KEY:-}"   # hex-encoded 32-byte AES-256 key; empty = disabled
```

Generate a key:

```bash
openssl rand -hex 32
```

:::caution
The encryption key must be identical on all gateway and relay instances.
:::

## Redis

```yaml
redis:
  addr: "${REDIS_ADDR:-redis:6379}"
  password: "${REDIS_PASSWORD:-}"
  db: 0
  pending_max_age: "${PENDING_MAX_AGE:-2h}"   # duration string; empty or "0s" = disabled
```

`pending_max_age` controls the stale-pending sweep (GC Phase 1): jobs still in `pending` state after this duration are marked failed and their S3 input files are deleted. Must be shorter than the S3 input object lifetime.

## Lifecycle

```yaml
lifecycle:
  persists_result: false        # true = keep records until TTL; false = delete on first GET or webhook
  job_ttl:
    global:    "${LIFECYCLE_JOB_TTL:-}"           # fallback for all statuses; empty = 2h safety net
    completed: "${LIFECYCLE_JOB_TTL_COMPLETED:-}" # override for completed jobs
    pending:   "${LIFECYCLE_JOB_TTL_PENDING:-}"   # override for pending/processing jobs
    failed:    "${LIFECYCLE_JOB_TTL_FAILED:-}"    # override for failed jobs
  gc:
    enabled:       ${LIFECYCLE_GC_ENABLED:-false}
    interval:      "${LIFECYCLE_GC_INTERVAL:-15m}"
    orphan_min_age: "${LIFECYCLE_GC_ORPHAN_MIN_AGE:-5m}"
```

### `persists_result`

When `false` (default): Redis record and S3 result file are deleted immediately after first consumption (first `GET /jobs/{id}` that returns a completed result, or first successful webhook delivery). Minimises storage usage.

When `true`: records survive until their TTL expires. Clients can re-fetch the result multiple times within the TTL window.

### `job_ttl`

Per-status Redis TTL for job records. `global` is the fallback for all statuses; per-status values take precedence. When all values are empty, an internal 2h safety net applies to orphaned records.

Example — keep completed results for 24h, fail quickly:

```yaml
lifecycle:
  job_ttl:
    global:    "2h"
    completed: "24h"
```

### `gc`

Background garbage collector — runs as a goroutine on a configurable ticker. Two phases per tick:

**Phase 1 — stale-pending sweep** (runs when `redis.pending_max_age` > 0):
Marks pending jobs older than `pending_max_age` as failed and deletes their S3 input files.

**Phase 2 — S3 orphan cleanup** (runs when `gc.enabled: true`):
Lists all S3 objects, groups by job ID (first path segment), and deletes any file whose Redis record has expired. Covers both `input_ref` and `result_ref` orphans — catches failed webhooks, `persists_result: true` jobs after TTL, and never-polled results.

Safety: if Redis `Ping` or `MGET` fails before any deletion, Phase 2 is aborted entirely and an error is logged. This prevents mass deletion when Redis is temporarily unavailable.

All `gc` parameters are hot-reload safe.

| Field | Default | Description |
|---|---|---|
| `gc.enabled` | `false` | Master switch — GC is off by default |
| `gc.interval` | `"15m"` | How often the GC runs |
| `gc.orphan_min_age` | `"5m"` | Minimum object age before orphan check — avoids deleting objects from in-flight uploads not yet registered in Redis |

## Services

```yaml
services:
  - type: audio                            # service type
    model: "whisper-large-v3"              # OpenAI model field
    default: true                          # fallback when model omitted

    # Sync routing
    operations:
      transcription:
        - "/v1/audio/transcriptions"
      translation:
        - "/v1/audio/translations"
    inference_url: "http://backend:80"     # base URL; original path appended

    # File validation (async mode only)
    accepted_exts: [".mp3", ".wav", ".m4a", ".ogg", ".flac"]
    max_file_size_mb: 500

    # Backend authentication (sync-direct only, optional)
    inference_headers:
      Authorization: "Bearer ${RERANKER_API_KEY}"
      X-Api-Key: "${BACKEND_KEY}"

    # Multi-backend routing (optional — replaces inference_url when set)
    # weight > 0 = eligible for weighted-random primary selection
    # weight = 0 = fallback-only (tried after all weight>0 backends fail)
    backends:
      - url: "http://backend-primary:8000"
        weight: 90
        model: "meta-llama/Meta-Llama-3-8B-Instruct"   # per-backend model override
        headers:
          Authorization: "Bearer ${PRIMARY_TOKEN}"       # per-backend header override
      - url: "http://backend-canary:8000"
        weight: 10
        model: "meta-llama/Meta-Llama-3.1-8B-Instruct"
        headers:
          Authorization: "Bearer ${CANARY_TOKEN}"

    # LLM proxy (optional — activates when provider is set)
    provider: passthrough          # openai | anthropic | ollama | passthrough
    backend_model: "meta-llama/Meta-Llama-3-8B-Instruct"  # default model rewrite; overridden by backends[].model
    response_cache_ttl: 3600       # seconds; 0 = disabled

    # Swagger spec (optional)
    swagger_url: "https://example.com/openapi.json"
    swagger_headers:
      Authorization: "Bearer ${TOKEN}"
      Accept: "application/octet-stream"
```

### Field reference

| Field | Required | Default | Description |
|---|---|---|---|
| `type` | yes | — | Service type, used in `/jobs/{service_type}` |
| `model` | no | `""` | OpenAI model field value for routing |
| `default` | no | `false` | Fallback model when request omits `model` |
| `operations` | no | `{}` | Map of operation name → URL paths |
| `inference_url` | no | `""` | Backend base URL for direct proxy (single backend, legacy — use `backends` for multi-backend) |
| `backends` | no | `[]` | List of backends with weighted routing. Takes precedence over `inference_url` when set. |
| `accepted_exts` | no | any | Allowed file extensions (e.g. `.mp3`) — async mode only |
| `max_file_size_mb` | no | `100` | Max upload size in MB |
| `inference_headers` | no | `{}` | HTTP headers injected on every sync-direct / LLM proxy request |
| `provider` | no | `""` | LLM provider: `openai`, `anthropic`, `ollama`, `passthrough` |
| `backend_model` | no | `""` | Backend model name — gateway rewrites the `model` field in the request |
| `response_cache_ttl` | no | `0` | Redis response cache TTL in seconds; `0` = disabled |
| `max_concurrent_sync` | no | `0` | Max simultaneous sync requests for this model across all replicas; `0` = unlimited. Returns `503` when full. |
| `token_limits` | no | `{}` | Per-user-type token budgets for LLM requests — see [Rate limiting](../configure/rate-limiting#token-budget-limiting) |
| `swagger_url` | no | `""` | URL to fetch an OpenAPI spec from |
| `swagger_headers` | no | `{}` | HTTP headers for `swagger_url` fetch |

### `backends`

Multi-backend list for blue/green, canary, or fallback routing. When set, takes precedence over `inference_url`.

```yaml
backends:
  - url: "http://backend-a:8000"
    weight: 90       # weighted-random primary selection
    model: "model-v1"   # overrides service-level backend_model for this backend
    headers:            # overrides service-level inference_headers for this backend
      Authorization: "Bearer ${TOKEN_A}"
  - url: "http://backend-b:8000"
    weight: 10
    model: "model-v2"
    headers:
      Authorization: "Bearer ${TOKEN_B}"
  - url: "http://backend-fallback:8000"
    weight: 0          # fallback-only: tried after all weight>0 backends fail
```

**Per-backend fields:**

| Field | Description |
|---|---|
| `url` | Backend URL (required) |
| `weight` | Routing weight. `0` = fallback-only (never primary-selected). |
| `model` | Overrides service-level `backend_model` for this backend only. |
| `headers` | HTTP headers injected on requests to this backend. Override `inference_headers`. |

On network error or 5xx, the next backend is tried. On 4xx (including 401), the retry loop stops — client errors are not retried.

### `inference_headers`

Arbitrary HTTP headers injected on every request forwarded to the inference backend. Only applies to the **sync-direct proxy** and **LLM proxy** flows. Has no effect on async jobs.

- Header values support `${VAR}` / `${VAR:-default}` env expansion.
- Config headers **override** any header with the same name sent by the client.
- `backends[].headers` takes further precedence over `inference_headers` for a specific backend.

```yaml
inference_headers:
  Authorization: "Bearer ${BACKEND_API_KEY}"
  X-Api-Key: "${BACKEND_KEY}"
```

## OpenTelemetry

```yaml
opentelemetry:
  enabled: false
  service_name: ""          # overrides the default "gatewai/gateway" resource attribute
  exporter:
    endpoint: "http://otel-collector:4318"
    insecure: false
    headers:
      Authorization: "Bearer ${OTEL_TOKEN}"
  traces:
    enabled: true
    sample_ratio: 1.0
    ignore_paths:           # path prefixes excluded from tracing (prefix-based)
      - /health             # default list — applied when ignore_paths is absent
      - /metrics
      - /docs
      - /openapi.yaml
  metrics:
    enabled: false
    interval: "60s"
  logs:
    enabled: false
```

| Field | Default | Description |
|---|---|---|
| `enabled` | `false` | Master switch — `false` = no-op providers, zero overhead |
| `service_name` | `"gatewai/gateway"` | OTel resource `service.name` attribute |
| `exporter.endpoint` | — | Base OTLP/HTTP endpoint for all signals |
| `exporter.insecure` | `false` | Skip TLS verification (dev only) |
| `traces.enabled` | `true` | Enable distributed tracing |
| `traces.sample_ratio` | `1.0` | Sampling rate: `1.0` = always, `0.1` = 10% |
| `traces.ignore_paths` | `[/health, /metrics, /docs, /openapi.yaml]` | Path prefixes excluded from tracing; overrides the default list entirely when set |
| `metrics.enabled` | `false` | Enable OTLP metrics push |
| `metrics.interval` | `"60s"` | Push interval |
| `logs.enabled` | `false` | Enable OTLP log export via slog bridge |

See [OpenTelemetry](../observe/opentelemetry.md) for the full guide (per-signal endpoint overrides, sampling, Helm sub-chart, backend examples).

## Hot reload

`POST /-/reload` re-reads the config file and atomically swaps the router. S3 and Redis connections are not re-initialised.

The `configmap-reload` sidecar can trigger this automatically on ConfigMap changes.

Parameters updated at runtime without pod restart:

| Parameter | Notes |
|---|---|
| `services.*` | Full registry rebuild |
| `lifecycle.*` (all fields) | Including `gc.enabled`, `gc.interval`, `gc.orphan_min_age`, `job_ttl.*` |
| `redis.pending_max_age` | Picked up on the next GC tick |
| `rate_limits` | Applied immediately to new requests |
| `audit_log` | Toggled without restart |
| `server.user_type_header` | Applied to new requests |

Not hot-reloadable (restart required): `redis.addr`, `s3.*`, `server.addr`, `server.*_timeout`, `metrics.top_consumers`.

## Usage tracking

```yaml
usage:
  retention: ""      # Go duration string; empty = all-time (no TTL)
```

Controls how long per-consumer cumulative usage data is kept in Redis sorted sets.

| Field | Description |
|---|---|
| `retention` | Duration before usage sorted sets expire. Empty or absent = no expiry (all-time accumulation). Accepts Go duration strings: `"720h"` (30 days), `"8760h"` (365 days). Note: `"d"` suffix is not a valid Go duration — use `"h"`. |

When `server.consumer_header` is not configured, a no-op tracker is used and no data is written. `retention` is hot-reloadable and affects new sorted-set keys only (existing keys keep their original TTL).

See [API reference](../reference/api) for the `GET /usage` and `GET /-/usage` endpoints that expose this data.
