# GatewAI

**GatewAI** is an AI inference job gateway for Kubernetes. It exposes an HTTP API that accepts file uploads, enqueues them as Redis jobs, and returns results asynchronously — or synchronously for low-latency use cases.

## Quick start

```bash
# Async job — fire and forget
curl -X POST https://your-gateway/jobs/audio \
  -F file=@audio.wav \
  -F model=whisper-large-v3

# Poll for result
curl https://your-gateway/jobs/audio/{job_id}

# Sync (OpenAI-compatible)
curl -X POST https://your-gateway/v1/audio/transcriptions \
  -F file=@audio.wav \
  -F model=whisper-large-v3

# LLM proxy (OpenAI SDK compatible)
curl -X POST https://your-gateway/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"Hello"}]}'
```

## Operating modes

| Mode | Endpoint | When to use |
|---|---|---|
| **Async** | `POST /jobs/{service_type}` | Large files, batch workloads, fire-and-forget |
| **Sync** | `POST /v1/*` | Low-latency, OpenAI-compatible clients |
| **LLM proxy** | `POST /v1/*` (JSON + `provider` set) | LLM APIs: OpenAI, Anthropic, vLLM, Ollama |

Sync mode routing:

- **Direct proxy** — HTTP proxy to `inference_url` (JSON or multipart)
- **LLM proxy** — provider translation, response caching, consumer metrics (JSON + `provider` in config)

## Components

Two independent Go binaries, two Docker images:

| Component | Image | Entry point |
|---|---|---|
| **Gateway** | `ghcr.io/qatr-io/gatewai/gateway` | `cmd/gateway/main.go` |
| **Relay** | `ghcr.io/qatr-io/gatewai/relay` | `relay/cmd/relay/main.go` |

The **relay** runs as a standalone Kubernetes Deployment alongside the inference pod. Each pod pops one job from a Redis list (`BLMOVE`), calls the local inference model, and exits — KEDA scales on Redis queue depth and terminates idle pods.

## Key features

- Config-driven service registry — add a new model with a YAML block, no code change
- Hot-reload via `POST /-/reload` — update config without pod restart
- Priority routing — Redis `LPUSH` inserts priority jobs at the head of the queue
- Consumer tracking — link jobs to API consumers via a configurable header
- **LLM proxy** — built-in OpenAI/Anthropic/Ollama/passthrough proxy with response caching and consumer token metrics
- **Rate limiting** — per-consumer Redis fixed-window limits, configurable per service type and user type
- **PII guardrails** — blocks LLM requests containing email, phone, IBAN, credit card, or SIREN/SIRET numbers
- **Audit trail** — structured per-request logs for LLM requests (opt-in)
- Prometheus metrics — requests, latency, tokens, cache hits, rate limits, top-N consumer usage, PII blocks
- OpenAPI 3.0 spec generated at runtime from the live registry
- AES-256-GCM at-rest encryption for S3 objects
- KEDA autoscaling — relay scales on Redis queue depth, scale-to-zero supported

## Links

- [Architecture overview](architecture/overview.md)
- [Helm deployment](deployment/helm.md)
- [Configuration reference](deployment/configuration.md)
- [Runbooks](runbooks/KeventGatewayHighErrorRate.md)
- [Changelog](changelog.md)
