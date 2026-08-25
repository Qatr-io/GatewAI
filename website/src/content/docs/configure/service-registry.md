---
title: Service registry
---

# Service registry

The gateway is entirely config-driven. Adding a new model or service type requires only a YAML block in `config.yaml` — no Go code change.

## Service entry fields

```yaml
services:
  - type: audio                        # service type (used in /jobs/{service_type})
    model: "whisper-large-v3"          # OpenAI "model" field value
    default: true                      # fallback when request omits model field

    # Sync / OpenAI-compatible
    operations:
      transcription:
        - "/v1/audio/transcriptions"   # all paths indexed; first used for async
      translation:
        - "/v1/audio/translations"
    inference_url: "http://..."        # backend base URL (path appended at runtime)

    # File validation (async mode only)
    accepted_exts: [".mp3", ".wav", ".m4a", ".ogg", ".flac"]
    max_file_size_mb: 500

    # Backend authentication — sync-direct only (optional)
    inference_headers:
      Authorization: "Bearer ${WHISPER_API_KEY}"
```

## Deprecating a model (`deprecated`)

```yaml
services:
  - type: audio
    model: "whisper-large-v2"
    deprecated: true                    # informational only — routing is unaffected
```

Setting `deprecated: true` on a service entry doesn't change routing or availability — it's purely a signal to API consumers, surfaced two ways:

- `GET /v1/models` returns `capabilities.deprecated: true` for that model.
- The generated OpenAPI spec (`/openapi.yaml`, `/docs`) marks the relevant operation with the standard [`deprecated: true`](https://swagger.io/docs/specification/v3_0/paths-and-operations/#deprecated-operations) field, which Swagger UI renders with a strikethrough.

Deprecation is applied at the finest granularity the spec allows:

- The per-model swagger doc served at `/swagger/{type}/{model}` (when `swagger_url` is configured) always marks its operations deprecated.
- A sync path shared by multiple models (e.g. several models behind the same `/v1/*` route) is only marked `deprecated: true` at the operation level once **every** model on that path is deprecated — otherwise still-active models on the same path would inherit a misleading warning. Until then, the deprecated model(s) are called out by name in the `model` field's description instead.

## Model resolution order

When a request omits the `model` field:

1. Single model registered for the type/path → auto-selected
2. Model marked `default: true` → used as fallback
3. Error listing available models

## Operations

The `operations` map replaces the old flat `openai_paths` list. Each key is an operation name; the value is a list of URL paths.

- **All paths** are indexed for sync routing (`POST /v1/*`)
- **First path** of the selected operation is embedded in the `InputEvent.InferenceURL` for async jobs
- The `operation` form field selects which operation to use for async submission (required when a model has multiple operations)

```bash
# Async: specify operation when model has multiple
curl -X POST /jobs/audio \
  -F file=@audio.wav \
  -F operation=translation

# Sync: operation is implicit from the URL path
curl -X POST /v1/audio/translations -F file=@audio.wav
```

## Multiple models per type

Multiple service entries may share the same `type` with different `model` values:

```yaml
services:
  - type: audio
    model: "whisper-large-v3"
    default: true
    ...

  - type: audio
    model: "whisper-large-v3-turbo"
    ...
```

The gateway routes by the `model` field in the request. The `default: true` flag designates the fallback.

## Path patterns with `{model}`

Paths can embed the model name directly in the URL:

```yaml
operations:
  infer:
    - "/v2/models/{model}/infer"
```

The gateway extracts the model name from the URL segment and routes accordingly. No `model` field is required in the request body.

## Backend authentication (`inference_headers`)

For sync-direct services whose backend requires authentication, add `inference_headers`:

```yaml
- type: reranker
  model: "bge-reranker-v2-m3"
  operations:
    rerank:
      - "/rerank"
  inference_url: "http://GatewAI-reranker-predictor.svc.cluster.local"
  inference_headers:
    Authorization: "Bearer ${RERANKER_API_KEY}"
```

Headers are injected on every outgoing request to the backend. Values support `${VAR}` expansion. Config headers override client headers with the same name.

!!! note
    `inference_headers` only applies to **sync-direct** proxy and **LLM proxy**. Async jobs processed by the relay are unaffected.

## LLM proxy services

When `provider` is set, JSON requests bypass the standard direct proxy and go through the built-in LLM proxy:

```yaml
- type: llm
  model: "gpt-4o"
  provider: openai            # openai | anthropic | ollama | passthrough
  backend_model: ""           # optional: rewrites model field sent to the backend
  response_cache_ttl: 3600    # seconds; 0 = disabled
  operations:
    chat:
      - "/v1/*"               # wildcard: matches all paths under /v1/
  inference_url: ""           # empty = provider default (e.g. https://api.openai.com)
  inference_headers:
    Authorization: "Bearer ${OPENAI_API_KEY}"
```

`backend_model` rewrites the `model` field in the JSON body before forwarding — useful for vLLM which expects HuggingFace model IDs. The cache key always uses the alias (`model` from config), not the backend name.

See [LLM proxy](llm-proxy.md) for full documentation.

## Sync concurrency cap (`max_concurrent_sync`)

To protect a backend from being overwhelmed by simultaneous sync requests, set `max_concurrent_sync` on the service entry:

```yaml
services:
  - type: llm
    model: "gpt-4o"
    provider: openai
    inference_url: "https://api.openai.com"
    max_concurrent_sync: 10   # at most 10 parallel sync calls for this model
    operations:
      chat:
        - "/v1/*"
```

The limit is enforced by a **shared Redis semaphore** (`gateway:semaphore:sync:{model}`) — the cap applies across all gateway replicas, not per pod. When all slots are busy, the gateway returns **`503 Service Unavailable`** immediately (no queuing).

- `0` (default) — no limit
- **Fail-open**: Redis errors are logged and the request is allowed through
- **Scope**: sync-direct and LLM-proxy requests only (`/v1/*`); async jobs are unaffected

The semaphore TTL is 30 minutes — a safety net that resets the counter if a gateway replica crashes while holding a slot.

### Reserving capacity for priority requests (`priority_reserved_sync`)

`priority_reserved_sync` carves out part of `max_concurrent_sync` exclusively for requests carrying the `server.priority_header` (the same header used for async queue-jump priority — see [priority routing](priority-routing.md)):

```yaml
services:
  - type: llm
    model: "gpt-4o"
    max_concurrent_sync: 10
    priority_reserved_sync: 3   # 3 of the 10 slots are reserved for priority requests
```

- `0` (default) — no reservation; priority requests compete in the shared pool like everyone else
- Non-priority requests only ever draw from the shared pool (`10 - 3 = 7` slots here)
- Priority requests try the reserved pool (`gateway:semaphore:sync:{model}:priority`) first, then fall back to the shared pool once the reserved pool is full — so a priority request is never worse off than a normal one
- This is capacity reservation, not queuing: a request still gets an immediate `200`/`503`, it just has a better chance of the former

## Hot reload

The service registry is reloaded atomically via `POST /-/reload`. The HTTP router is swapped with the new registry. Infrastructure (S3, Redis) is not re-initialised.
