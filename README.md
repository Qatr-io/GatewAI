# GatewAI

API Gateway for KServe inference services. Each service supports multiple operating modes:

| Mode | Endpoints | When to use |
|---|---|---|
| **Async** (Redis queue) | `POST /jobs/{service_type}`, `GET /jobs/{service_type}/{id}`, `GET /jobs` | Large files, long-running tasks (>30s), webhook delivery |
| **Sync direct proxy** | `POST /v1/*` (JSON or multipart) | OpenAI SDK integration, sync-only services (reranker, embeddings…) |
| **LLM proxy** | `POST /v1/*` JSON + `provider` configured | LLM proxying (OpenAI, Anthropic, Ollama, vLLM) with caching and metrics |

## Architecture

### Async mode

```
Client
  │
  ▼
POST /jobs/{service_type} (multipart: file, model, operation?)
  │
  ├─ 1. File → S3
  ├─ 2. Job record → Redis (status: pending)
  └─ 3. Job ID → Redis list relay:<model>:pending  (RPUSH)
                          │
                          ▼
                    Relay Deployment (one job per pod lifecycle)
                                              │
                                              ├─ BLMOVE relay:<model>:pending → relay:<model>:processing
                                              ├─ Download file from S3
                                              ├─ POST multipart → GPU model (127.0.0.1:9000/<inference_url>)
                                              ├─ Upload result.json → S3
                                              └─ PUBLISH jobs:<model>:completed  (Redis pub/sub)
                                                                    │
                                              ┌─────────────────────┘
                                              │  (gateway internal consumer.Manager)
                                              ▼
                                       Redis updated (status: completed/failed)
                                       + Webhook POST if callback_url provided
Client
  │
  ▼
GET /jobs/{service_type}/{id}  →  { status, result (inline JSON) }
```

### Sync mode (OpenAI-compatible proxy)

**Direct proxy** (`application/json` or `multipart/form-data` request):
```
Client  POST /v1/*
  │
  ▼
Gateway → HTTP proxy → inference_url + original path → GPU model
  │
  ▼ (response streamed directly)
Client
```

**LLM proxy** (`application/json` request + service with `provider` configured):
```
Client  POST /v1/chat/completions  {"model": "my-alias", ...}
  │
  ▼
Gateway — LLM proxy
  ├── Redis cache check (SHA-256 key of canonical body)  ── HIT → response + X-Cache: HIT
  │                                                                        ↑
  ├── MISS → for each backend (weighted-random order):         (async goroutine, 5s)
  │     ├── Rewrite model alias → backend.model (or backend_model)
  │     ├── Inject backend.headers (override inference_headers)
  │     ├── Translate request (if anthropic: OpenAI → Messages API)
  │     ├── Forward to backend URL
  │     └── Network error / 5xx → next backend ; 4xx → stop
  ├── Translate response → OpenAI format
  ├── Token metrics + consumer tracking (Redis sorted set)
  └── Client response  X-Cache: MISS  +  async cache-fill

  Streaming (`"stream": true`): SSE piped directly, no caching or translation.
  Retry possible before WriteHeader; impossible once the stream has started.
```

### Required external components

| Component | Role | Required |
|---|---|---|
| **Redis** | Job state, relay queue, completion pub/sub, LLM cache, rate limiting | Always |
| **S3** | Storage for input files and results | Always |

---

## Quick start

### Prerequisites

- Go 1.23+
- Redis and an S3-compatible bucket accessible

### Build

```bash
# Gateway
go build -ldflags "-X main.version=v0.4.11" -o gateway ./cmd/gateway
CONFIG_PATH=/etc/GatewAI/config.yaml ./gateway

# Relay
cd relay
go build -o relay ./cmd/relay
CONFIG_PATH=/etc/relay/config.yaml ./relay
```

### Docker

```bash
docker build -t gatewai-gateway .
docker run \
  -e S3_ACCESS_KEY=... \
  -e S3_SECRET_KEY=... \
  -e REDIS_ADDR=redis:6379 \
  -p 8080:8080 \
  gatewai-gateway
```

---

## Configuration

Configuration is read from `config.yaml` (default path). All values of the form `${VAR:-default}` are substituted from the environment at startup.

### Gateway (`config.yaml`)

```yaml
server:
  addr: ":8080"
  read_timeout: 120s    # high for large uploads
  write_timeout: 0s     # 0 = disabled — required for sync mode (long inference)
  idle_timeout: 120s
  # consumer_header: HTTP header injected by APISIX after auth (e.g. "X-Consumer-Username").
  # Enables consumer tracking: GET /jobs, job isolation, per-consumer metrics.
  # Leave empty if no upstream auth.
  consumer_header: "${CONSUMER_HEADER:-}"
  # priority_header: HTTP header for priority routing (e.g. "X-Priority").
  # If present, can be used for application-level priority routing.
  priority_header: "${PRIORITY_HEADER:-}"
  # user_type_header: HTTP header for user type (e.g. "X-User-Type" → "sa" | "user").
  # Used for rate limiting and LLM metrics labelling.
  user_type_header: "${USER_TYPE_HEADER:-}"

s3:
  endpoint: "https://s3.fr-par.scw.cloud"
  region: "fr-par"
  access_key: "${S3_ACCESS_KEY}"
  secret_key: "${S3_SECRET_KEY}"
  bucket: "GatewAI-jobs"

encryption:
  key: "${ENCRYPTION_KEY:-}"    # AES-256-GCM at-rest, empty = disabled

redis:
  addr: "redis:6379"
  password: ""
  db: 0
  job_ttl_hours: 72

# High-cardinality metrics — consumer token tracking
metrics:
  top_consumers: 10      # expose top-N in Prometheus via Redis sorted sets; 0 = disabled

# Per-consumer, per-service-type, per-user-type rate limiting (Redis fixed-window).
# rate / period              → max requests per time window.
# token_rate / token_period  → max total tokens (prompt+completion) per window
#                              (LLM proxy only; rate: 0 or absent = no limit).
# max_concurrent             → max async jobs in pending+processing state at once (0 = unlimited).
# processing_time / processing_period → max cumulative inference seconds per window (0 = unlimited).
rate_limits:
  audio:
    unlimited:           # rate: 0 = no limit (Redis not consulted)
      rate: 0
    sa:                  # user_type_header = "sa"
      rate: 100
      period: 1m
      max_concurrent: 10          # at most 10 jobs in flight simultaneously
      processing_time: 7200       # at most 2 h of inference per hour
      processing_period: 1h
    user:
      rate: 20
      period: 1m
    "*":                 # fallback if user_type absent or not listed
      rate: 10
      period: 1m
  llm:
    sa:
      rate: 1000
      period: 1m
      token_rate: 5000000    # 5 M tokens / hour
      token_period: 1h
    "*":
      rate: 60
      period: 1m
      token_rate: 100000     # 100 k tokens / hour
      token_period: 1h

services:
  - type: audio
    model: "whisper-large-v3"
    default: true           # model used by default if unspecified and multiple models are configured
    operations:
      transcription:
        - "/v1/audio/transcriptions"
      translation:
        - "/v1/audio/translations"
    inference_url: "http://GatewAI-transcription-predictor.default.svc.cluster.local"
    accepted_exts: [".mp3", ".wav", ".m4a", ".ogg", ".flac"]
    max_file_size_mb: 500

  - type: ocr
    model: "deepseek-ocr"
    default: true
    operations:
      ocr:
        - "/v1/ocr"
        - "/v1/vision/ocr"    # alias — all paths of an operation are indexed
    inference_url: "http://GatewAI-ocr-predictor.default.svc.cluster.local"
    accepted_exts: [".pdf", ".jpg", ".jpeg", ".png", ".tiff", ".bmp"]
    max_file_size_mb: 50

  # Sync-direct only service — POST /v1/* → direct proxy to inference_url.
  # POST /jobs/{service_type} → 405 Method Not Allowed.
  - type: reranker
    model: "bge-reranker-v2-m3"
    operations:
      rerank:
        - "/rerank"
    inference_url: "http://GatewAI-reranker-predictor.default.svc.cluster.local"
    # No async (no relay queue) → sync-direct only

  # LLM proxy — openai, anthropic, ollama or passthrough (vLLM…)
  # JSON POST /v1/* requests go through the LLM proxy (cache, metrics, translation).
  - type: llm
    model: "chat-smart"                # client-facing alias
    provider: passthrough              # openai | anthropic | ollama | passthrough
    backend_model: "meta-llama/Meta-Llama-3-8B-Instruct"  # empty = alias forwarded as-is
    response_cache_ttl: 3600           # seconds; 0 = disabled
    operations:
      chat:
        - "/v1/*"                      # wildcard: all OpenAI-compatible paths
    # Multi-backend: blue/green, canary, fallback
    # weight > 0 = weighted-random selection ; weight = 0 = fallback only
    backends:
      - url: "http://vllm-primary.default.svc.cluster.local:8000"
        weight: 90
        model: "meta-llama/Meta-Llama-3-8B-Instruct"
        headers:
          Authorization: "Bearer ${VLLM_PRIMARY_TOKEN}"
      - url: "http://vllm-canary.default.svc.cluster.local:8000"
        weight: 10
        model: "meta-llama/Meta-Llama-3.1-8B-Instruct"
        headers:
          Authorization: "Bearer ${VLLM_CANARY_TOKEN}"
    # inference_url: "" (legacy — single backend, replaced by backends[])
    # inference_headers applies to all backends; backends[].headers override it
```

#### `services[]` fields

| Field | Description |
|---|---|
| `type` | Service type name (e.g. `audio`, `ocr`). Multiple entries can share the same type with different models. |
| `model` | Model identifier, forwarded in the OpenAI payload for routing. |
| `default` | `true` → default model for this type when no `model` is specified in the request. |
| `operations` | Map `operation_name → list of URL paths`. All paths are indexed for sync routing; the first is used as `inference_url` in async jobs. |
| `inference_url` | Backend base URL for direct proxy. The original request path is appended. |
| `accepted_exts` | Accepted file extensions (async mode only). Empty or absent = all extensions accepted. |
| `max_file_size_mb` | Maximum file size. Absent or 0 = 100 MB default. |
| `backends` | List of backends with weighted routing. Takes precedence over `inference_url`. See below. |
| `inference_headers` | HTTP headers injected on every request to the backend (sync-direct and LLM proxy). Supports `${VAR}`. Overridden by `backends[].headers`. |
| `provider` | Enables the LLM proxy: `openai`, `anthropic`, `ollama`, `passthrough`. Absent = standard direct proxy. |
| `backend_model` | Model name forwarded to the backend (default for all backends). Overridden by `backends[].model`. |
| `response_cache_ttl` | Redis cache TTL in seconds. `0` = disabled. |
| `swagger_url` | URL to the service's OpenAPI JSON spec. Optional — if absent, the service does not appear in the `/docs` dropdown. |

#### `backends[]` fields

| Field | Description |
|---|---|
| `url` | Backend URL (**required**) |
| `weight` | Routing weight. `0` = fallback only (never selected as primary). |
| `model` | Overrides `backend_model` for this backend only — useful for canary deployments. |
| `headers` | HTTP headers injected on requests to this backend. Override `inference_headers`. |

### Relay (`relay/config.yaml`)

```yaml
redis:
  addr: "${REDIS_ADDR:-redis:6379}"
  password: "${REDIS_PASSWORD:-}"
  db: 0

model: "${RELAY_MODEL}"          # e.g. whisper-large-v3  — sets relay:<model>:pending queue name

queue_pop_timeout: "${QUEUE_POP_TIMEOUT:-5m}"   # BLMOVE timeout before the pod exits 0

s3:
  endpoint:   "${S3_ENDPOINT:-https://s3.fr-par.scw.cloud}"
  region:     "${S3_REGION:-fr-par}"
  access_key: "${S3_ACCESS_KEY}"
  secret_key: "${S3_SECRET_KEY}"
  bucket:     "${S3_BUCKET:-GatewAI-jobs}"

encryption:
  key: "${ENCRYPTION_KEY:-}"

# Base URL of the local inference container (same pod).
# The OpenAI path is provided by the gateway in InputEvent.inference_url
# and appended dynamically: base_url + inference_url.
inference:
  base_url: "http://127.0.0.1:${INFERENCE_PORT:-9000}"
  api_key:  ""
  timeout:  "300s"
  extra_fields:             # optional form fields added to every multipart request
    response_format: "json"
    # language: "en"
    # prompt: "..."
```

### Environment variables (gateway)

| Variable | Default | Description |
|---|---|---|
| `CONFIG_PATH` | `config.yaml` | Path to the configuration file |
| `S3_ENDPOINT` | `https://s3.fr-par.scw.cloud` | S3 endpoint |
| `S3_REGION` | `fr-par` | Region |
| `S3_ACCESS_KEY` | — | Access Key ID (**required**) |
| `S3_SECRET_KEY` | — | Secret Key (**required**) |
| `S3_BUCKET` | `GatewAI-jobs` | Bucket name |
| `REDIS_ADDR` | `redis:6379` | Redis address |
| `REDIS_PASSWORD` | _(empty)_ | Redis password |
| `ENCRYPTION_KEY` | _(empty)_ | Hex-encoded AES-256-GCM key (32 bytes) |
| `CONSUMER_HEADER` | _(empty)_ | HTTP header to identify the consumer (e.g. `X-Consumer-Username`) |
| `PRIORITY_HEADER` | _(empty)_ | HTTP header for priority routing (e.g. `X-Priority`) |
| `USER_TYPE_HEADER` | _(empty)_ | HTTP header for user type (e.g. `X-User-Type`) — rate limiting + LLM metrics |

### Environment variables (relay)

| Variable | Default | Description |
|---|---|---|
| `CONFIG_PATH` | `config.yaml` | Path to the configuration file |
| `RELAY_MODEL` | — | Model name (**required**) — determines the `relay:<model>:pending` queue |
| `QUEUE_POP_TIMEOUT` | `5m` | BLMOVE timeout before the pod exits with code 0 |
| `INFERENCE_PORT` | `9000` | Local model server port |
| `REDIS_ADDR` | `redis:6379` | Redis address |
| `REDIS_PASSWORD` | _(empty)_ | Redis password |
| `S3_ACCESS_KEY` | — | Access Key ID (**required**) |
| `S3_SECRET_KEY` | — | Secret Key (**required**) |
| `ENCRYPTION_KEY` | _(empty)_ | Must match the gateway value |

---

## Helm — gateway deployment

```bash
helm upgrade --install gatewai-gateway ./helm/gateway \
  -f values.yaml \
  --namespace default
```

### Key values (`values.yaml`)

```yaml
image:
  repository: ghcr.io/qatr-io/gatewai/gateway
  tag: v0.15.0

config:
  redis:
    addr: "redis:6379"
  s3:
    endpoint: "https://s3.fr-par.scw.cloud"
    bucket: "GatewAI-jobs"

services:
  - type: audio
    model: "whisper-large-v3"
    default: true
    operations:
      transcription:
        - "/v1/audio/transcriptions"
      translation:
        - "/v1/audio/translations"
    inferenceURL: "http://GatewAI-transcription-predictor.default.svc.cluster.local"
    acceptedExts: [".mp3", ".wav", ".m4a", ".ogg", ".flac"]
    maxFileSizeMB: 500
```

---

## Adding an inference service

No code change required. Simply add a block to the gateway's `config.yaml` and deploy a Relay Deployment configured with the correct `model`.

**Gateway `config.yaml`** (async + sync service):

```yaml
services:
  - type: audio
    model: "pyannote-audio-3.1"
    operations:
      diarization:
        - "/v1/audio/diarizations"
    inference_url: "http://GatewAI-diarization-predictor.default.svc.cluster.local"
    accepted_exts: [".mp3", ".wav", ".m4a", ".ogg", ".flac"]
    max_file_size_mb: 500
```

The Redis queue `relay:pyannote-audio-3.1:pending` is created automatically on first job submission. No prior topic configuration is needed.

**Gateway `config.yaml`** (sync-direct only service):

```yaml
services:
  - type: reranker
    model: "bge-reranker-v2-m3"
    operations:
      rerank:
        - "/rerank"
    inference_url: "http://GatewAI-reranker-predictor.default.svc.cluster.local"
    # No relay → sync-direct only
    # POST /jobs/reranker → 405  |  POST /rerank → direct proxy
```

**Relay** (`relay/config.yaml` or ConfigMap):

```yaml
model: "pyannote-audio-3.1"   # determines the relay:pyannote-audio-3.1:pending queue
redis:
  addr: "${REDIS_ADDR:-redis:6379}"
```

> **Multiple models per type**: multiple entries can share the same `type` with different `model` values. The gateway routes based on the `model` field in the request. The `default: true` field designates the model used when `model` is absent and multiple models are configured.

> **Multiple operations per model**: a single model can expose multiple operations (e.g. transcription and translation) via `operations`. In async mode, specify the operation with `-F operation=transcription` when the model offers more than one.

> **Sync-direct service**: a service without a relay is handled entirely as a direct proxy — no Redis queue is created for that type.

---

## API

### Interactive documentation

The gateway generates the OpenAPI 3.0 spec at startup from the service registry:

- **Swagger UI**: `GET /docs` — multi-spec dropdown: gateway (async/sync jobs) + one tab per service with a `swagger_url`
- **Gateway spec**: `GET /openapi.yaml` — dynamically generated spec (async + sync routes)
- **Service spec**: `GET /swagger/{type}/{model}` — inference service OpenAPI spec, cached at startup

### Sync mode — OpenAI-compatible endpoints

These endpoints are exposed dynamically based on the `operations` configured in `config.yaml`.

#### `POST /v1/audio/transcriptions` — Audio transcription

**Content-Type**: `multipart/form-data`

| Field | Type | Required | Description |
|---|---|---|---|
| `model` | string | if multiple models | E.g. `whisper-large-v3`. Optional if only one model or a default is configured. |
| `file` | file | yes | Audio file (.mp3, .wav, .m4a, .ogg, .flac) |

```bash
curl https://api.GatewAI.example.com/v1/audio/transcriptions \
  -F model=whisper-large-v3 \
  -F file=@interview.wav
```

**With the OpenAI Python SDK**

```python
from openai import OpenAI

client = OpenAI(base_url="https://api.GatewAI.example.com", api_key="unused")

with open("interview.wav", "rb") as f:
    transcript = client.audio.transcriptions.create(
        model="whisper-large-v3",
        file=f,
    )
print(transcript.text)
```

---

#### `POST /v1/ocr` — OCR (documents, images)

**Content-Type**: `multipart/form-data`

| Field | Type | Required | Description |
|---|---|---|---|
| `model` | string | if multiple models | E.g. `deepseek-ocr` |
| `file` | file | yes | Document (.pdf, .jpg, .jpeg, .png, .tiff, .bmp) |

```bash
curl https://api.GatewAI.example.com/v1/ocr \
  -F model=deepseek-ocr \
  -F file=@document.pdf
```

---

### Async mode — Jobs

#### `POST /jobs/{service_type}` — Submit a job

**Content-Type**: `multipart/form-data`

| Field | Type | Required | Description |
|---|---|---|---|
| `model` | string | if multiple models with no default | E.g. `whisper-large-v3`. Optional if only one model or `default: true` is configured. |
| `operation` | string | if multiple operations | E.g. `transcription` or `translation`. Optional if only one operation for the model. |
| `file` | file | yes | File to process |
| `callback_url` | string | no | URL called via POST when the job completes |

**Response** `202 Accepted`

```json
{
  "job_id": "550e8400-e29b-41d4-a716-446655440000",
  "service_type": "audio",
  "model": "whisper-large-v3",
  "status": "pending"
}
```

```bash
# Explicit model and operation
curl -X POST http://localhost:8080/jobs/audio \
  -F "model=whisper-large-v3" \
  -F "operation=transcription" \
  -F "file=@interview.wav" \
  -F "callback_url=https://my-app.example.com/hooks/inference"

# Default model, single operation → optional fields
curl -X POST http://localhost:8080/jobs/audio \
  -F "file=@interview.wav"
```

---

#### `GET /jobs/{service_type}/{id}` — Job status

**Response** `200 OK`

```json
{
  "job_id": "550e8400-e29b-41d4-a716-446655440000",
  "service_type": "audio",
  "model": "whisper-large-v3",
  "status": "completed",
  "result": { "text": "Hello, welcome to this meeting..." },
  "created_at": "2026-03-05T10:00:00Z",
  "updated_at": "2026-03-05T10:04:32Z"
}
```

Example for a pending job:

```json
{
  "job_id": "550e8400-e29b-41d4-a716-446655440000",
  "service_type": "audio",
  "model": "whisper-large-v3",
  "status": "pending",
  "queue_position": 3,
  "created_at": "2026-03-05T10:00:00Z",
  "updated_at": "2026-03-05T10:00:00Z"
}
```

| Field | Description |
|---|---|
| `status` | `pending` \| `processing` \| `completed` \| `failed` |
| `queue_position` | 1-indexed position in the model's queue (present only if `pending`) |
| `result` | Inference result JSON payload (present only if `completed`) |
| `error` | Error message (present only if `failed`) |

> **Note**: the S3 result file is deleted after this call — subsequent calls return 404.

> **Consumer isolation**: if `consumer_header` is configured and the header is present in the request, the job must belong to the identified consumer — otherwise `404` (no information leak about other consumers' jobs). Requests without a header (admin, internal use) bypass this check.

---

#### `GET /jobs` — List a consumer's jobs

Requires `consumer_header` to be configured. Returns the paginated list of jobs for the consumer identified by the header, sorted by creation date descending.

**Query params**: `limit` (default 20, max 100), `offset` (default 0)

**Response** `200 OK`

```json
{
  "consumer": "alice",
  "total": 42,
  "limit": 20,
  "offset": 0,
  "jobs": [
    {
      "job_id": "550e8400-...",
      "service_type": "audio",
      "model": "whisper-large-v3",
      "status": "completed",
      "created_at": "2026-03-05T10:04:32Z",
      "updated_at": "2026-03-05T10:04:32Z"
    }
  ]
}
```

```bash
curl http://localhost:8080/jobs \
  -H "X-Consumer-Username: alice" \
  "?limit=10&offset=0"
```

> If `consumer_header` is not configured, returns `501 Not Implemented`.

**Simple polling**

```bash
JOB_ID="550e8400-e29b-41d4-a716-446655440000"
while true; do
  RESPONSE=$(curl -s http://localhost:8080/jobs/audio/$JOB_ID)
  STATUS=$(echo $RESPONSE | jq -r '.status')
  [ "$STATUS" = "completed" ] && echo $RESPONSE | jq '.result' && break
  [ "$STATUS" = "failed" ]    && echo "Error: $(echo $RESPONSE | jq -r '.error')" && break
  sleep 10
done
```

---

### `GET /health`

```json
{ "status": "ok", "time": "2026-03-05T10:00:00Z" }
```

---

### `GET /metrics`

Prometheus metrics in text format (compatible with Prometheus / VictoriaMetrics scraping).

---

## Async contract (Redis queue)

The gateway and relay communicate via Redis. Job data is stored as Redis JSON; input/output files are in S3.

### Job record — stored by the gateway in Redis

```json
{
  "job_id": "550e8400-e29b-41d4-a716-446655440000",
  "service_type": "audio",
  "model": "whisper-large-v3",
  "status": "pending",
  "input_ref": "550e8400-.../input.wav",
  "inference_url": "/v1/audio/transcriptions",
  "created_at": "2026-03-05T10:00:00Z"
}
```

| Field | Description |
|---|---|
| `input_ref` | S3 object key of the input file |
| `inference_url` | Path to call on the local model (appended to the relay's `inference.base_url`) — derived from the first path of the chosen operation |

### Queue sequence

```
Gateway   RPUSH relay:<model>:pending <job_id>
Relay     BLMOVE relay:<model>:pending relay:<model>:processing LEFT RIGHT
Relay     [processes the job]
Relay     HSET job:<id> status completed result_ref ...
Relay     PUBLISH jobs:<model>:completed <job_id>
Relay     LREM relay:<model>:processing 1 <job_id>
Gateway   [receives PUBLISH, updates Redis, triggers webhook]
```

---

## Webhook (optional, async mode)

If `callback_url` is provided at submission, the gateway performs a `POST` to that URL as soon as the job transitions to `completed` or `failed`. On HTTP failure (5xx or timeout), 3 attempts are made with exponential backoff (2s → 4s → 8s).

```json
{
  "job_id": "550e8400-e29b-41d4-a716-446655440000",
  "service_type": "audio",
  "status": "completed",
  "result_ref": "550e8400-.../result.json",
  "completed_at": "2026-03-05T10:04:32Z"
}
```

---

## Monitoring

Both components expose Prometheus metrics at `GET /metrics`.

### Gateway

| Metric | Type | Labels | Description |
|---|---|---|---|
| `GatewAI_requests_total` | counter | `mode`, `service_type`, `model`, `status` | Handled requests (mode `async` or `sync`, HTTP code in `status`) |
| `GatewAI_request_duration_seconds` | histogram | `mode`, `service_type`, `model` | End-to-end handler latency |
| `GatewAI_s3_operation_duration_seconds` | histogram | `operation` (upload/get/delete) | S3 operation latency |
| `GatewAI_s3_errors_total` | counter | `operation` | S3 errors |
| `GatewAI_redis_operation_duration_seconds` | histogram | `operation` (save_job/get_job/delete_job/update_job_result/push_queue) | Redis operation latency |
| `GatewAI_redis_errors_total` | counter | `operation` | Redis errors |
| `GatewAI_jobs_by_consumer_total` | counter | `mode`, `service_type`, `model`, `consumer` | Jobs submitted per consumer (only if `consumer_header` configured) |
| `GatewAI_llm_requests_total` | counter | `service_type`, `model`, `backend_model`, `provider`, `user_type`, `status` | LLM proxy requests |
| `GatewAI_llm_request_duration_seconds` | histogram | `service_type`, `model`, `backend_model`, `provider`, `user_type` | LLM proxy latency |
| `GatewAI_llm_tokens_total` | counter | `service_type`, `model`, `backend_model`, `user_type`, `type` | Tokens consumed (`prompt`/`completion`) |
| `GatewAI_llm_tokens_per_request` | histogram | `service_type`, `model`, `backend_model`, `user_type` | Token distribution per request |
| `GatewAI_llm_consumer_tokens_top` | gauge | `consumer`, `user_type`, `type` | Top-N consumers by tokens (Redis, if `metrics.top_consumers > 0`) |
| `GatewAI_cache_hits_total` | counter | `service_type`, `model` | LLM cache hits |
| `GatewAI_cache_misses_total` | counter | `service_type`, `model` | LLM cache misses |
| `GatewAI_cache_errors_total` | counter | `service_type`, `model`, `op` | LLM cache errors |
| `GatewAI_ratelimit_requests_total` | counter | `service_type`, `user_type`, `result` | Rate limit checks (`allowed`/`rejected`) |
| `GatewAI_ratelimit_consumer_hits_total` | counter | `service_type`, `user_type` | Consumers that exceeded their limit |
| `GatewAI_ratelimit_errors_total` | counter | `service_type` | Redis errors during rate limiting |
| `GatewAI_token_ratelimit_checked_total` | counter | `service_type`, `user_type`, `result` | Token budget checks (`allowed`/`rejected`) |
| `GatewAI_token_ratelimit_errors_total` | counter | `service_type` | Redis errors during token budget checks |

### Relay

| Metric | Type | Labels | Description |
|---|---|---|---|
| `GatewAI_relay_jobs_total` | counter | `service_type`, `status` (completed/failed) | Jobs processed |
| `GatewAI_relay_inference_duration_seconds` | histogram | `service_type` | Local inference API call duration |
| `GatewAI_relay_input_size_bytes` | histogram | `service_type` | Size of input files downloaded from S3 |
| `GatewAI_relay_s3_operation_duration_seconds` | histogram | `operation` (get/put/delete) | S3 operation latency |
| `GatewAI_relay_s3_errors_total` | counter | `operation` | S3 errors |
| `GatewAI_relay_redis_publish_errors_total` | counter | — | Redis pub/sub publish errors (jobs completed) |
| `GatewAI_relay_redis_done_errors_total` | counter | — | Errors removing the job from the processing list |

### Example Prometheus configuration

```yaml
scrape_configs:
  - job_name: gatewai-gateway
    static_configs:
      - targets: ["gatewai-gateway.default.svc.cluster.local:8080"]

  - job_name: gatewai-relay
    static_configs:
      - targets: ["gatewai-relay.default.svc.cluster.local:8080"]
```

---

## Project structure

```
.
├── cmd/gateway/main.go          # Entry point — wiring and graceful shutdown
├── internal/
│   ├── config/config.go         # YAML loading + env variable expansion
│   ├── model/job.go             # Shared types: Job, InputEvent, ResultEvent
│   ├── service/registry.go      # Config-driven registry (sync + async routing, default per type)
│   ├── storage/
│   │   ├── s3.go                # S3 client (AWS SDK v2)
│   │   └── redis.go             # Job persistence (JSON blob + TTL) + RPUSH/LPUSH queue
│   ├── consumer/
│   │   └── manager.go           # Redis pub/sub subscriptions (jobs:<model>:completed)
│   ├── ratelimit/
│   │   └── ratelimit.go         # Redis fixed-window rate limiting (Lua INCR+EXPIRE)
│   ├── cache/
│   │   ├── cache.go             # Cache interface + Redis entry
│   │   ├── key.go               # SHA-256 canonical key of the LLM body
│   │   └── redis.go             # Redis implementation
│   ├── llmproxy/
│   │   ├── handler.go           # LLM proxy: cache → provider → translate → async cache-fill
│   │   └── provider/            # openai, anthropic, ollama, passthrough
│   ├── metrics/
│   │   ├── metrics.go           # Prometheus definitions (promauto) — GET /metrics
│   │   └── consumer_tracker.go  # ConsumerTracker interface + Redis sorted-set + top-N refresh
│   └── handler/
│       ├── jobs.go              # POST /jobs/{service_type}  •  GET /jobs/{service_type}/{id}
│       ├── sync.go              # POST /v1/*  (direct proxy or LLM proxy)
│       ├── docs.go              # GET /docs (Swagger UI)  •  GET /openapi.yaml (dynamically generated spec)
│       ├── health.go            # GET /health
│       └── middleware.go        # Structured logger (slog/JSON)
├── relay/                       # Relay Deployment (separate Go module: gatewai/relay)
│   ├── cmd/relay/main.go
│   ├── internal/
│   │   ├── config/config.go     # Relay config: model, redis, inference.base_url + extra_fields
│   │   ├── queue/               # BLMOVE pop, Publish (pub/sub), Done (remove from processing)
│   │   ├── store/               # GetJob, UpdateJobResult (Redis JSON)
│   │   ├── relay/               # Async job processing
│   │   ├── metrics/             # Relay Prometheus definitions — GET /metrics
│   │   ├── adapter/             # Generic multipart adapter (model + extra_fields + file)
│   │   └── storage/             # S3 client
│   └── config.yaml              # Config template (env vars expanded at startup)
├── helm/gateway/                # Gateway Helm chart (includes Redis-HA)
├── k8s/                         # Kubernetes manifests (Relay Deployment, KEDA ScaledObject)
├── config.yaml                  # Default gateway configuration
└── Dockerfile                   # Multi-stage build → distroless image (~10 MB)
```

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for the gitflow, branch conventions, and release process.
