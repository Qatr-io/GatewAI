# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Repository overview

**GatewAI** is an AI inference job gateway for Kubernetes. It exposes an HTTP API that accepts file uploads, enqueues them as Kafka jobs, and returns results asynchronously. A relay Deployment runs alongside each inference pod, pulling jobs from Kafka, calling the local inference model, and publishing results back.

Two independent Go modules (separate `go.mod`, separate Docker images):
- **Gateway** — root module `gatewai/gateway`, entry point `cmd/gateway/main.go`
- **Relay** — module `gatewai/relay` in `./relay/`, entry point `relay/cmd/relay/main.go`

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

Current tags: gateway `v0.13.0`, relay `v0.6.0`.

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
  └── Publish InputEvent → Kafka jobs.<model>.input
                                    │
                        Relay Deployment (pull consumer)
                             ├── FetchMessage from Kafka
                                    ▼
                              ├── Download file from S3
                              ├── POST to local inference model (127.0.0.1:9000/<inference_url>)
                              ├── Upload result.json → S3
                              └── Publish ResultEvent → jobs.<model>.results
                                                                │
                                                         Gateway ConsumerManager
                                                              ├── Update Redis
                                                              ├── Notify Redis pub/sub (job:<id>:done)
                                                              └── Trigger webhook (if callback_url set)
```

**Sync direct proxy** (`POST /v1/*`):
```
Gateway → HTTP proxy → InferenceService URL (inference_url in config)
```

### Sync (OpenAI-compatible) mode — routing summary

| Request | Path |
|---|---|
| Any `POST /v1/*` | Direct proxy to `inference_url` |

Configured via `services[].operations`, `services[].model`, `services[].inference_url` in `config.yaml`.

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

### Dynamic OpenAPI spec

`handler.GenerateSpec(registry, version)` builds the full OpenAPI 3.0.3 spec at startup from the live registry. No static file — the spec always reflects the current config. Served at:
- `GET /openapi.yaml` — raw spec
- `GET /docs` — Swagger UI

Version injected at build time: `go build -ldflags "-X main.version=v0.4.3" ./cmd/gateway`.

### Config loading

Both binaries use `config.Load(path)` which reads a YAML file and expands `${VAR}` / `${VAR:-default}` with `os.Expand` before unmarshalling. The config path defaults to `config.yaml` in the working directory, overridden by env var `CONFIG_PATH`.

**Adding a new service type** requires only a new entry in `config.yaml` (and `values.yaml` for Helm). No Go code change is needed — the service registry (`internal/service/registry.go`) is entirely config-driven.

### Kafka authentication (SASL/TLS)

`github.com/segmentio/kafka-go` v0.4.47 is used for both reading and writing.

- Readers use `kafkago.Dialer` (built by `internal/kafka/auth.go:buildDialer`)
- Writers use `kafkago.Transport` (built by `internal/kafka/auth.go:buildTransport`)

**Critical**: `buildTransport` returns `(*kafkago.Transport, error)` where transport can be `nil` when SASL/TLS is not configured. Never assign the result directly to `w.Transport` (a `RoundTripper` interface) — a typed nil pointer produces a non-nil interface value and panics. Use:
```go
if p.transport != nil {
    w.Transport = p.transport
}
```

Broker: `default-kafka-bootstrap.infra-kafka.svc.cluster.local:9093` (SASL_SSL, SCRAM-SHA-512).

### Strimzi KafkaUsers (`k8s/kafka-users.yaml`)

- `gatewai-gateway` (namespace `infra-kafka`) — Write/Read on `jobs.*` topics, Read on `gatewai-gateway*` groups
- `gatewai-relay` (namespace `infra-kafka`) — Read on `jobs.*` topics, **Read + Describe + Delete** on `inference-*` groups (Delete is required by the Knative controller for ConsumerGroup finalization), Write on `jobs.*` topics

### Secret hygiene

Strimzi-generated secrets (e.g. `gatewai-relay-kafka`) must not have trailing newlines in values. The Knative KafkaSource controller does exact string comparison on `sasl-type` — a `\n` suffix causes `[protocol SASL_SSL] unsupported SASL mechanism`. Verify with:
```bash
kubectl get secret gatewai-relay-kafka -n default \
  -o jsonpath='{.data.sasl-type}' | base64 -d | xxd
```

## Key files

| File | Purpose |
|---|---|
| `config.yaml` | Gateway config template (env-expanded at startup) |
| `relay/config.yaml` | Relay config template |
| `values.yaml` | Helm values for production deployment |
| `helm/gateway/` | Helm chart — generates ConfigMap, Secret, Deployment, Ingress |
| `k8s/kafka-users.yaml` | Strimzi KafkaUser ACLs (apply in `infra-kafka` namespace) |
| `k8s/deployment-transcription.yaml` | Deployment + Service + RBAC for whisper-large-v3 |
| `internal/service/registry.go` | Config-driven service registry (routing, default model, operations map) |
| `internal/handler/docs.go` | Dynamic OpenAPI spec generator + Swagger UI handler |
| `internal/kafka/auth.go` | SASL/TLS dialer+transport construction for gateway |
| `relay/internal/kafka/auth.go` | SASL/TLS transport construction for relay |

## Deployment

Helm chart deploys the gateway with Redis-HA (HAProxy front-end). The relay runs as a standalone Deployment alongside the inference pod (see `k8s/deployment-transcription.yaml`), not managed by Helm.

The Helm chart is published to GitHub Pages at `https://ia-generative.github.io/GatewAI` (auto-updated on push to `main` when `helm/` changes). The `gh-pages` branch must exist in the repository.

```bash
# Add Helm repo
helm repo add GatewAI https://ia-generative.github.io/GatewAI
helm repo update
helm install gatewai-gateway GatewAI/gatewai-gateway -f values.yaml

# Or deploy from local sources
helm upgrade --install gatewai-gateway ./helm/gateway -f values.yaml

# Apply Strimzi users (namespace infra-kafka)
kubectl apply -f k8s/kafka-users.yaml -n infra-kafka

# Apply InferenceService + ServingRuntime
kubectl apply -f k8s/deployment-transcription.yaml
```
