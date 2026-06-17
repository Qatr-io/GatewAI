# GatewAI

API Gateway for KServe inference services on Kubernetes — async job queue, OpenAI-compatible sync proxy, and LLM proxy in a single binary.

| Mode | Endpoint | When to use |
|---|---|---|
| **Async** | `POST /jobs/{service_type}` | Large files, long-running tasks (>30s), webhook delivery |
| **Sync** | `POST /v1/*` | OpenAI SDK integration, low-latency services |
| **LLM proxy** | `POST /v1/*` (JSON + `provider`) | OpenAI / Anthropic / Ollama / vLLM with caching and metrics |

## Architecture

![Architecture overview](docs/architecture/overview.png)

## Features

- Config-driven service registry — add a model with a YAML block, no code change
- Hot-reload via `POST /-/reload`
- Priority routing (Redis `LPUSH`)
- Consumer tracking via configurable header
- LLM proxy — OpenAI / Anthropic / Ollama / passthrough (vLLM), provider translation
- Response cache (Redis, SHA-256 key, `X-Cache` header)
- Multi-backend routing — blue/green, canary, fallback with weighted selection
- Per-consumer rate limiting — fixed-window, per service type and user type, request + token budgets
- PII guardrails — blocks LLM requests containing email, phone, IBAN, credit card, SIREN/SIRET
- Audit trail — structured slog per LLM request (opt-in)
- AES-256-GCM at-rest encryption for S3 objects
- KEDA autoscaling — relay scales on Redis queue depth, scale-to-zero
- Prometheus metrics — requests, latency, tokens, cache, rate limits, top-N consumer usage

## Documentation

Full configuration reference, deployment guide, API reference, and runbooks:
**[qatr-io.github.io/GatewAI](https://qatr-io.github.io/GatewAI)**
