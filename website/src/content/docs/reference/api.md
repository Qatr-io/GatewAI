---
title: API reference
---

# API reference

All endpoints are served by the gateway on `:8080` by default. No authentication is enforced at the gateway level — secure via your API gateway (APISIX, ingress, etc.).

## Async jobs

### Submit a job

```
POST /jobs/{service_type}
```

Accepts a multipart form. Returns `202 Accepted` with a job ID.

**Form fields**

| Field | Required | Description |
|-------|----------|-------------|
| `file` | yes | Input file |
| `model` | no | Target model. Auto-selected if only one is registered for the type |
| `operation` | no | Operation name (e.g. `transcription`). Required if the model has multiple operations |
| `callback_url` | no | Webhook URL called on completion |

**Response**

```json
{ "job_id": "01HXYZ..." }
```

---

### Get job status / result

```
GET /jobs/{service_type}/{id}
```

Returns the job record. When status is `completed`, the `result` field contains the inference output.

**Response fields**

| Field | Description |
|-------|-------------|
| `id` | Job ID |
| `status` | `pending` \| `processing` \| `completed` \| `failed` \| `cancelled` |
| `service_type` | Service type |
| `model` | Model used |
| `result` | Inference result (only when `completed`) |
| `error` | Error message (only when `failed`) |
| `created_at` | ISO 8601 timestamp |

If `server.consumer_header` is configured and the header is present, ownership is enforced: a consumer can only access their own jobs.

---

### Cancel a job

```
DELETE /jobs/{service_type}/{id}
```

Returns `202 Accepted`. If the job is already processing, a cancellation signal is sent to the relay — the relay stops inference asynchronously. The job record is kept until TTL expires.

---

### List jobs

```
GET /jobs
```

Lists the caller's jobs across all service types. Requires `server.consumer_header` to be configured — returns `400` otherwise.

**Query parameters**

| Parameter | Description |
|-----------|-------------|
| `service_type` | Filter by service type (optional) |
| `status` | Filter by status (optional) |

---

## Sync proxy

### OpenAI-compatible proxy

```
POST /v1/*
```

Proxies the request to the configured `inference_url`. Handles both JSON and multipart requests. When `provider` is set on the service, routes through the LLM proxy (OpenAI/Anthropic/Ollama/passthrough).

---

### List models

```
GET /v1/models
```

Returns an OpenAI-compatible model list for all registered services with a `model` field set.

---

## Usage

### Get my usage

```
GET /usage
```

Returns cumulative totals and current-window values for the calling consumer across all service types.

Requires `server.consumer_header` to be configured — returns `501 Not Implemented` if absent. Returns `400 Bad Request` if the header is missing from the request.

**Response**

```json
{
  "consumer": "alice",
  "retention": "all-time",
  "last_active": "2026-06-29T10:55:00Z",
  "usage": [
    {
      "service_type": "llm",
      "total": {
        "requests": 1250,
        "tokens": { "prompt": 45000, "completion": 12000 }
      },
      "window": {
        "requests": 42,
        "tokens": 8500,
        "reset_at": "2026-06-29T11:00:00Z"
      }
    },
    {
      "service_type": "audio",
      "total": {
        "requests": 80,
        "jobs": 80,
        "processing_time_seconds": 3600.0
      },
      "window": {
        "requests": 5,
        "processing_time_seconds": 150.5,
        "reset_at": "2026-06-29T11:00:00Z"
      }
    }
  ]
}
```

**Response fields**

| Field | Description |
|---|---|
| `consumer` | Consumer identifier from the consumer header |
| `retention` | Retention period for cumulative data: `"all-time"` or a duration string (e.g. `"8760h"`) |
| `last_active` | Timestamp of the consumer's last tracked request (omitted if no activity) |
| `usage[].service_type` | Service type (matches `services[].type` in config) |
| `usage[].total.requests` | All-time (or retention-window) cumulative request count |
| `usage[].total.jobs` | Async jobs submitted (async service types only) |
| `usage[].total.processing_time_seconds` | Cumulative inference seconds (async only) |
| `usage[].total.tokens` | LLM token counts (`prompt` + `completion`) — `llm` service type only |
| `usage[].window` | Current rate-limit window values; omitted when no active limit exists |
| `usage[].window.reset_at` | When the current window expires |

Service types with zero cumulative data are omitted. `window` is omitted when no rate-limit key exists in Redis (no limit configured or window expired).

---

### List all consumers' usage (admin)

```
GET /-/usage
```

Returns usage for all consumers, paginated and filterable. This is an admin endpoint in the `/-/` namespace — caller is responsible for upstream authentication.

**Query parameters**

| Parameter | Default | Description |
|---|---|---|
| `consumer` | — | Filter to a single consumer (returns a single-element list) |
| `type` | — | Filter by service type; paginates consumers ranked by request count |
| `limit` | `20` | Max results per page (max `100`) |
| `offset` | `0` | Pagination offset |

**Response**

```json
{
  "total": 150,
  "limit": 20,
  "offset": 0,
  "consumers": [
    {
      "consumer": "alice",
      "retention": "all-time",
      "last_active": "2026-06-29T10:55:00Z",
      "usage": [...]
    }
  ]
}
```

---

## Admin

### Hot reload config

```
POST /-/reload
```

Reloads `config.yaml` from disk without restarting the process. Returns `200` on success, `500` if the new config is invalid (the old config remains active).

---

### Purge jobs

```
POST /-/jobs/purge
```

Deletes all job records and their S3 objects for a given model. Intended for maintenance/cleanup.

**Request body (JSON)**

```json
{ "model": "whisper-large-v3" }
```

---

## Docs

| Endpoint | Description |
|----------|-------------|
| `GET /docs` | Swagger UI for the gateway API |
| `GET /openapi.yaml` | Raw OpenAPI 3.0.3 spec (generated at startup from the live registry) |
| `GET /docs/spec/{type}/{model}` | Swagger UI for the inference backend spec (requires `swagger_url` in service config) |

---

## System

| Endpoint | Description |
|----------|-------------|
| `GET /health` | Returns `200 OK`. Used by Kubernetes readiness/liveness probes |
| `GET /metrics` | Prometheus metrics in text exposition format |
