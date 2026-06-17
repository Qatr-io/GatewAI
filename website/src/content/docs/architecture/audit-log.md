---
title: Audit log
---

# Audit log

GatewAI can emit a structured `slog` record for every LLM proxy request. Disabled by default to avoid unexpected log volume.

## Configuration

```yaml
audit_log:
  enabled: true
  prompt: false   # include raw request body — opt-in only
```

`audit_log` is a top-level config key (not per-service).

| Field | Default | Description |
|---|---|---|
| `enabled` | `false` | Emit one log record per LLM request |
| `prompt` | `false` | Include the raw request body as a `prompt` field. Opt-in only — the body may contain PII. |

## Log fields

Each record is a single `slog.InfoContext` call with the message `"llm request"` and the following key-value pairs:

| Field | Type | Description |
|---|---|---|
| `service_type` | string | Service type (e.g. `llm`) |
| `model` | string | Client-facing model alias |
| `backend_model` | string | Model name forwarded to the backend |
| `provider` | string | Provider: `openai`, `anthropic`, `ollama`, `passthrough` |
| `consumer` | string | Consumer name from `consumer_header`; empty if not configured |
| `user_type` | string | User type from `user_type_header`; empty if not configured |
| `status` | int | HTTP status code returned to the client |
| `duration_ms` | int64 | End-to-end request duration in milliseconds |
| `backend_url` | string | Backend URL that served the request |
| `stream` | bool | `true` if the request used SSE streaming |
| `prompt_tokens` | int | Prompt token count (absent for streaming responses) |
| `completion_tokens` | int | Completion token count (absent for streaming responses) |
| `prompt` | string | Raw request body (only present when `audit_log.prompt: true`) |

## Example log entry

```json
{
  "time": "2026-03-05T10:04:32.123Z",
  "level": "INFO",
  "msg": "llm request",
  "service_type": "llm",
  "model": "chat-smart",
  "backend_model": "meta-llama/Meta-Llama-3-8B-Instruct",
  "provider": "passthrough",
  "consumer": "alice",
  "user_type": "user",
  "status": 200,
  "duration_ms": 842,
  "backend_url": "http://vllm-primary.default.svc.cluster.local:8000",
  "stream": false,
  "prompt_tokens": 312,
  "completion_tokens": 128
}
```

## PII warning

!!! warning
    Setting `prompt: true` writes the full request body to your log output. If clients send messages containing PII, those payloads will appear in logs.

    Consider pairing `prompt: true` with [PII guardrails](guardrails.md), which blocks PII-containing requests before they reach the backend — and therefore before they are logged.
