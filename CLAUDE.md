# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Repository overview

**GatewAI** is an AI inference job gateway for Kubernetes. It exposes an HTTP API that accepts file uploads, enqueues them as Redis jobs, and returns results asynchronously. A relay Deployment runs alongside each inference pod, pulling jobs from a Redis queue, calling the local inference model, and publishing results back.

Two independent Go modules (separate `go.mod`, separate Docker images):
- **Gateway** — root module `gatewai/gateway`, entry point `cmd/gateway/main.go`
- **Relay** — module `gatewai/relay` in `./relay/`, entry point `relay/cmd/relay/main.go`

## Memory (MemPalace)

You have persistent memory via MemPalace MCP tools. Your memory survives across sessions.

### ALWAYS do this:
1. **On first message of a session:** Call `mempalace_status` to check your palace, then `mempalace_search` for context about the user and current topic.
2. **When the user tells you something to remember:** Call `mempalace_add_drawer` to store it AND `mempalace_kg_add` to add it to the knowledge graph. Confirm you saved it.
3. **When asked about past conversations or facts:** Call `mempalace_search` or `mempalace_kg_query` BEFORE answering. Never guess from session context alone.
4. **At the end of meaningful conversations:** Call `mempalace_diary_write` to record what happened.


## Build commands

```bash
# Gateway
go build ./cmd/gateway          # from repo root
go vet ./...
go test ./...

# Relay
cd relay
go build ./cmd/relay
go vet ./...
go test ./...
```

### Docker images & releases

Images are hosted on **ghcr.io** and released via GitHub Actions. To release:

```bash
# 1. Create a release branch from develop
git checkout develop && git pull
git checkout -b release/vX.Y.Z

# 2. Bump versions and update changelog
#    - helm/gateway/values.yaml  → image.tag
#    - helm/gateway/Chart.yaml   → version + appVersion
#    - CHANGELOG.md              → new section
#    - k8s/deployment-transcription.yaml → relay image tag (relay releases only)

# 3. Commit, push and open a PR → main
git add ... && git commit -m "chore(release): gateway vX.Y.Z"
git push -u origin release/vX.Y.Z
gh pr create --base main --title "release: gateway vX.Y.Z"

# 4. After the PR is merged, tag on main and push
git checkout main && git pull
git tag gateway/vX.Y.Z && git push origin gateway/vX.Y.Z
# relay: git tag relay/vX.Y.Z && git push origin relay/vX.Y.Z

# 5. Merge main back into develop to keep them in sync
git checkout develop && git merge --no-ff main
git push origin develop
```

Images:
- Gateway:    `ghcr.io/qatr-io/gatewai/gateway:vX.Y.Z`
- Relay: `ghcr.io/qatr-io/gatewai/relay:vX.Y.Z`

Current tags: gateway `v0.21.0`, relay `v0.12.0`.

## Architecture

### Request flow

**Async** (`POST /jobs/{service_type}`):
```
Client
  │  POST /jobs/{service_type} (multipart: file, model?, operation?)
  ▼
Gateway (:8080)
  ├── Upload file → S3
  ├── Save job record → Redis (TTL 72h)
  └── RPUSH job → Redis queue relay:<model>:pending
                                    │
                        Relay Deployment (pull consumer)
                             ├── BLMOVE from relay:<model>:pending
                                    ▼
                              ├── Download file from S3
                              ├── POST to local inference model (127.0.0.1:9000/<inference_url>)
                              ├── Upload result.json → S3
                              └── PUBLISH → Redis pub/sub jobs:<model>:completed
                                                                │
                                                         Gateway Subscriber
                                                              ├── Update Redis
                                                              ├── Notify Redis pub/sub (job:<id>:done)
                                                              └── Trigger webhook (if callback_url set)
```

**Async crash recovery (lease + reaper)**: on `BLMOVE` the relay writes a per-job lease `relay:<model>:lease:<id>` (config `lease_ttl`, default 60s) and refreshes it every `lease_ttl/3` while processing; `Done` deletes it. If the relay pod dies mid-job the lease expires and the gateway GC's reaper (`ReapOrphanedProcessingJobs`, phase 0 of `runGC`) requeues the abandoned `relay:<model>:processing` entry to `pending` — atomically re-checking the lease (Lua) so a live worker's job is never touched, and idempotently across replicas. After `lifecycle.gc.max_reap_attempts` requeues (default 3) the job is dead-lettered to `relay:<model>:deadletter` and marked failed. Metric: `gatewai_async_jobs_reaped_total{model, outcome}`.
**Durable webhooks** (`internal/consumer/webhook.go` + `webhook_retry.go`): `Send` makes one inline delivery attempt; on failure (5xx/network) the retry is persisted to Redis (ZSET `webhook:retries` + per-job task key `webhook:retry:{id}`) and worked by `RunRetryLoop` with exponential backoff (`webhooks.retry_backoff`→`max_backoff`), so a gateway restart never drops pending retries. Claims are atomic (Lua, visibility-timeout) so the 2 replicas don't double-send. After `webhooks.max_retries` (default 3) attempts the webhook is dead-lettered to `webhook:deadletter`. Metrics: `gatewai_webhook_deliveries_total{result}`, `gatewai_webhook_retry_queue_depth`. When `webhooks.signing_secret` is set, each delivery carries `X-Gatewai-Signature: t=<unix>,v1=HMAC-SHA256(secret,"<t>.<body>")` for authenticity + replay protection.
**Idempotency** (async submit): `POST /jobs/{type}` honours an optional `Idempotency-Key` header. The key is reserved in Redis (`idem:{consumer}:{key}`, SETNX, `jobs.idempotency_ttl` default 24h) against the new job's ID just before `SaveJob`; a repeat with the same key returns the original job (`200` + `X-Idempotent-Replay: true`) instead of a duplicate inference, or `409` if that job is gone. Metric `gatewai_idempotency_requests_total{service_type, outcome}`.

**Job poll — expired vs not-found**: `GET /jobs/{type}/{id}` returns `410 Gone` + `{"status":"expired"}` when the job record TTL has passed but a long-lived tombstone (`jobmeta:{id}`, `jobs.expired_marker_ttl`, default 7d, written by `SaveJob`) proves it existed; a truly unknown id still returns `404`.

**Sync direct proxy** (`POST /v1/*`):
```
Gateway → HTTP proxy → InferenceService URL (inference_url in config)
```

### Sync (OpenAI-compatible) mode — routing summary

| Request | Path |
|---|---|
| Any `POST /v1/*` | Direct proxy to `inference_url` (or LLM proxy if `provider` set) |

Configured via `services[].operations`, `services[].model`, `services[].inference_url` in `config.yaml`.

**Request body limit**: the sync JSON path caps the body at `server.max_body_mb` MiB (default 1 MiB), enforced with `http.MaxBytesReader` — oversized requests get `413`. Raise it for vision models sending base64-embedded images (base64 inflates ~33%). Independent of `services[].max_file_size_mb`, which bounds only the multipart upload path.

**LLM proxy** (`internal/llmproxy/`): when `provider` is set on a service, the gateway translates and proxies LLM requests instead of passing them through raw. Providers: `openai`, `anthropic` (full OpenAI ↔ Anthropic Messages API translation), `ollama`, `passthrough` (vLLM and OpenAI-compatible backends).

- **Model aliases**: `backend_model` rewrites the `model` field before forwarding (e.g. `"gpt-4o"` → `"meta-llama/Meta-Llama-3-8B-Instruct"` for vLLM). The real backend model is surfaced to clients in `GET /v1/models` as `backend_model` (and `backend_models[]` when backends serve distinct models) — default-on, no config.
- **Response cache**: Redis exact-match cache keyed on SHA-256 of request body, configurable TTL via `response_cache_ttl`; `stream=true` and `Cache-Control: no-cache` bypass cache; `X-Cache: HIT/MISS` on every response
- **Wildcard routing**: paths ending with `/*` (e.g. `/v1/*`) register as chi wildcard routes — proxies all sub-paths without enumerating them

### Service registry — key concepts

**`operations` map** (`map[string][]string`): replaces the old `openai_paths` flat list. Each service entry maps operation names to URL paths. All paths are indexed for sync routing; the first path of the selected operation is used as `inference_url` in async `InputEvent`.

```yaml
operations:
  transcription:
    - "/v1/audio/transcriptions"
  translation:
    - "/v1/audio/translations"
```

**`default: true`**: designates the fallback model for a service type when the request omits the `model` field and multiple models are registered. Resolution order:
1. Explicit `model` field → exact lookup
2. Single model registered for the type/path → auto-selected
3. Model marked `default: true` → fallback
4. Error listing available models

**`operation` form field** (async only): selects the operation when a model has multiple operations (`-F operation=transcription`). Auto-selected if only one operation is configured.

**Multiple models per type**: multiple service entries may share the same `type` with different `model` values. The gateway routes by `model` field in the request.

**`visibility`** (`services[].visibility`): gates a model to an audience — `user_types` (matched against `server.user_type_header`) and/or `groups` (from the authed `Principal`). A restricted model is fail-closed: filtered out of `GET /v1/models` and returns `404` on `/v1/*`, `/jobs/{service_type}`, and `GET /v1/models?model=` for callers outside its audience (indistinguishable from non-existent; anonymous callers see only public models). Enforced in `handler.checkModelVisible` on all three paths and as a list filter in `ListModels`. Enables beta-testing a model through the same API. Composes with `policies` (both must pass). Metric `gatewai_model_hidden_total{service_type, model}`. Registry helpers: `Def.IsRestricted()`, `Def.VisibleTo(userType, groups)`, `Def.BackendModelNames()`.

### Dynamic OpenAPI spec

`handler.GenerateSpec(registry, version)` builds the full OpenAPI 3.0.3 spec at startup from the live registry. No static file — the spec always reflects the current config. Served at:
- `GET /openapi.yaml` — raw spec
- `GET /docs` — Swagger UI

Version injected at build time: `go build -ldflags "-X main.version=v0.4.3" ./cmd/gateway`.

### Config loading

Both binaries use `config.Load(path)` which reads a YAML file and expands `${VAR}` / `${VAR:-default}` with `os.Expand` before unmarshalling. The config path defaults to `config.yaml` in the working directory, overridden by env var `CONFIG_PATH`.

**Adding a new service type** requires only a new entry in `config.yaml` (and `values.yaml` for Helm). No Go code change is needed — the service registry (`internal/service/registry.go`) is entirely config-driven.

### Lifecycle GC

`cmd/gateway/gc.go`: unified background GC, gated by `lifecycle.gc.enabled` (default `false`), ticking every `lifecycle.gc.interval` (default 15m). Three phases per cycle:

1. **Stale-pending sweep** (`redis.SweepStalePendingJobs`) — marks pending jobs older than `redis.pending_max_age` as failed and deletes their S3 input. Skipped when `pending_max_age` is `0s`/empty.
2. **Orphan S3 cleanup** — deletes S3 objects whose job ID predates `lifecycle.gc.orphan_min_age` and no longer has a Redis job record.
3. **Orphan relay queue cleanup** (`redis.SweepOrphanedRelayQueueEntries`) — removes job IDs from `relay:{model}:pending`/`relay:{model}:processing` whose Redis job record no longer exists. Catches jobs a relay pod left behind when it `os.Exit`s on an infra error before calling `Done` (queue.go) — with one-pod-per-job scaling, no later pod ever cleans that entry up, so it inflates `gatewai_relay_queue_depth` forever without this sweep. Swept counts are exposed as `gatewai_relay_queue_orphans_swept_total{model,state}`.

Phases 2 and 3 abort the cycle if Redis is unreachable (`redis.Ping`), since "job gone" can't be distinguished from "Redis down".

### Rate limiting & token limits

**`rate_limits`** (top-level config block): per-consumer, per-service-type, per-user-type request limits. Keyed by `{consumer}:{service_type}:{user_type}` in Redis. Returns `429` with `Retry-After` on breach. Requires `server.consumer_header` and `server.user_type_header`.

```yaml
server:
  consumer_header: "X-Consumer-Username"   # set by APISIX after auth
  user_type_header: "X-User-Type"          # set by OPA (sa | user | ...)

rate_limits:
  llm:
    sa:    { rate: 100, period: 1m }
    user:  { rate: 20,  period: 1m }
    "*":   { rate: 10,  period: 1m }   # fallback
```

**`token_limits`** (top-level config block): per-consumer, per-service-type token budget limits for LLM proxy. Optimistic enforcement (tokens known post-response); streaming skips counting.

### Guardrails

`internal/guardrails/`: PII and secrets detection for LLM JSON requests, applied on the sync LLM-proxy path (`POST /v1/*`) per service. Configurable pipeline with a per-service **action** and a set of **check groups**:

- **Action** (`guardrails.action`): `block` (reject `422`, default), `redact` (mask matches in-place with placeholders like `[REDACTED_EMAIL]` and forward the cleaned body), or `flag` (log + metric only, forward unchanged).
- **Check groups** (`guardrails.checks`): `pii` (universal: email, credit card, IBAN, IPv4, E.164 phone), `pii_fr` (phone FR, NIR, SIREN/SIRET), `pii_us` (SSN), `pii_uk` (NINO), `pii_es` (DNI), `pii_it` (Codice Fiscale), `secrets` (AWS key, private-key block, JWT, GitHub/Slack/Google tokens). Country groups are strictly opt-in.
- **Stages**: top-level `checks`/`action` = **input** (request). An optional nested `guardrails.output` block (`checks`/`action`) is the **output** stage — scans/redacts the model response (`choices[*].message.content`) in the LLM proxy (`internal/llmproxy/handler.go`). Streaming responses always degrade to `flag` (cannot redact/block mid-stream).

Numeric national-ID patterns (NIR, SIREN/SIRET, SSN, DNI) have higher false-positive rates — enable the relevant country group per service after assessing payloads. Prometheus counters: `gatewai_guardrails_total{service_type, model, stage, action, result}` (`stage` = `input`|`output`) and the legacy `gatewai_guardrails_pii_blocked_total{service_type, model}`.

### Authentication

`internal/auth/`: optional gateway-side authentication. **Absent `auth` block ⇒ no gateway auth** — identity is trusted from upstream headers (the default when an upstream reverse proxy handles auth). One mode per deployment via `auth.mode`:
- **`oauth2`** — validates OAuth2 **access tokens** (resource-server model, *not* OIDC id-tokens): JWT signature via cached JWKS (issuer discovery), `iss`/`aud`/`exp` checks, configurable claim mapping (`scope`/`groups`/`roles`/`consumer`). **Fails closed** (`401` invalid/missing, `503` if JWKS unreachable). Strips the client bearer before proxying. Resolves a `Principal{Subject,Consumer,Groups,Roles,Scopes,UserType}` into request context and bridges consumer/user_type into the headers downstream rate-limiting/ownership already read (after stripping inbound values — anti-spoof).
- **`proxy`** — trusts identity headers set by an upstream reverse proxy, now incl. groups/roles.

`/health` `/metrics` `/docs` `/openapi.yaml` are exempt. Auth config changes require a restart (not hot-reloaded). Deps: `golang-jwt/jwt/v5` + `MicahParks/keyfunc/v3` (no OIDC lib — access-token validation only). Token format via `auth.oauth2.validation` (`auto` default | `jwt` | `introspection`): JWTs are verified locally via JWKS; opaque tokens via RFC 7662 **introspection** (`auth.oauth2.introspection` — client creds, result cache capped by token exp, gives live revocation); `auto` picks per token shape. See **Access control** below for model/role authz and per-group quotas.

### Access control

`internal/authz/`: optional default-deny model/service access control, enabled by the top-level `policies` block (requires `auth.mode` — it needs a `Principal`). `policies.rules` are allow-rules: a rule grants a request when its `match` intersects the caller (any-of within each non-empty field of `groups`/`roles`/`scopes`/`consumers`/`user_types`; empty match = everyone) AND the requested model matches the rule's `allow_models` globs (and `allow_service_types` if set). No granting rule → `403`. `default: allow` disables enforcement. Enforced on sync (`/v1/*`) and async (`/jobs`) after routing resolves the model, reading the `Principal` from context. Metric: `gatewai_authz_decisions_total{service_type, model, decision}`. Hot-reloadable; absent `policies` = no enforcement.

A rule may also carry an optional **`limits`** block (a `RateLimitConfig`: `rate`/`period`, `token_rate`/`token_period`) — a **per-group quota** applied per-member (keyed by consumer) on the sync LLM path. The matched rule's limits are stashed in the request context (`ratelimit.WithPolicyLimits`) and enforced by the existing limiter (`rlp:`/`trlp:` keys), coexisting with `rate_limits`/`token_limits` (both must pass). Anonymous callers are skipped. (Per-group concurrent/processing-time and async enforcement are follow-ups.)

### Service headers

`services[].headers`: static headers injected on every outgoing request to the backend. Values support `${VAR}` expansion. Config headers override client headers with the same name.


## Key files

| File | Purpose |
|---|---|
| `config.yaml` | Gateway config template (env-expanded at startup) |
| `relay/config.yaml` | Relay config template |
| `values.yaml` | Helm values for production deployment |
| `helm/gateway/` | Helm chart — generates ConfigMap, Secret, Deployment, Ingress |
| `k8s/deployment-transcription.yaml` | Deployment + Service + RBAC for whisper-large-v3 |
| `internal/service/registry.go` | Config-driven service registry (routing, default model, operations map) |
| `internal/handler/docs.go` | Dynamic OpenAPI spec generator + Swagger UI handler |
| `cmd/gateway/gc.go` | Unified background GC — stale-pending sweep, S3 orphan cleanup, relay queue orphan cleanup |
| `internal/ratelimit/` | Per-consumer Redis fixed-window rate limiting |
| `internal/consumer/` | Redis pub/sub subscriber + webhook sender (replaces Kafka) |
| `internal/llmproxy/` | LLM proxy with provider interface (openai/anthropic/ollama/passthrough) |
| `internal/cache/` | Redis response cache for LLM proxy |
| `internal/guardrails/` | PII detection for LLM requests |
| `internal/concurrency/` | Distributed sync semaphore via Redis |
| `relay/internal/queue/` | Redis BLMOVE queue consumer |

## Deployment

Helm chart deploys the gateway with Redis-HA (HAProxy front-end). The relay runs as a standalone Deployment alongside the inference pod (see `k8s/deployment-transcription.yaml`), not managed by Helm.

The Helm chart is published to GitHub Pages at `https://qatr-io.github.io/GatewAI` (auto-updated on push to `main` when `helm/` changes). The `gh-pages` branch must exist in the repository.

```bash
# Add Helm repo
helm repo add gatewai https://qatr-io.github.io/GatewAI
helm repo update
helm install gatewai-gateway gatewai/gatewai-gateway -f values.yaml

# Or deploy from local sources
helm upgrade --install gatewai-gateway ./helm/gateway -f values.yaml

# Apply InferenceService + ServingRuntime
kubectl apply -f k8s/deployment-transcription.yaml
```
