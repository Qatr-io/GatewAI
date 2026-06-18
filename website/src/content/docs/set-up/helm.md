---
title: Helm chart
---

# Helm chart

The gateway is packaged as a Helm chart and published to GitHub Pages.

## Install

```bash
helm repo add GatewAI https://ia-generative.github.io/GatewAI
helm repo update

helm install gatewai-gateway GatewAI/gatewai-gateway -f values.yaml
```

## Upgrade

```bash
helm upgrade gatewai-gateway GatewAI/gatewai-gateway -f values.yaml
```

## Install from local sources

```bash
helm upgrade --install gatewai-gateway ./helm/gateway -f values.yaml
```

## Minimal `values.yaml`

```yaml
image:
  tag: "v0.14.0"

config:
  s3:
    endpoint: "https://s3.fr-par.scw.cloud"
    region: "fr-par"
    bucket: "my-GatewAI-jobs"
  redis:
    addr: "redis:6379"

secrets:
  s3AccessKey: "my-access-key"
  s3SecretKey: "my-secret-key"
```

## Redis HA

The chart deploys Redis with Redis-HA (HAProxy front-end) by default. Redis is required for job state, rate limiting, and result caching.

## ConfigMap hot reload

The chart supports `configmap-reload` sidecar to trigger `POST /-/reload` automatically when the ConfigMap changes:

```yaml
configReloader:
  enabled: true
  image: ghcr.io/jimmidyson/configmap-reload:v0.14.0
```

## Ingress

```yaml
ingress:
  enabled: true
  className: nginx
  host: GatewAI.example.com
  tls:
    enabled: true
    secretName: GatewAI-tls
```

## Extra environment variables

```yaml
extraEnvVars:
  - name: ENCRYPTION_KEY
    valueFrom:
      secretKeyRef:
        name: GatewAI-encryption
        key: key
```

## Deploy relay (Deployment + KEDA)

The relay runs as a standalone Deployment alongside the inference container. Each pod processes one job and exits — KEDA creates a new pod when the Redis queue is non-empty.

Example manifests for a Whisper transcription service are in [`examples/relay-transcription/`](https://github.com/Qatr-io/GatewAI/tree/main/examples/relay-transcription/). Copy and adapt them to your cluster, then apply:

```bash
# ConfigMap (relay config.yaml)
kubectl apply -f examples/relay-transcription/relay-cm.yaml

# Deployment (2 containers: inference model + relay)
kubectl apply -f examples/relay-transcription/deployment.yaml

# KEDA ScaledObject (scales on Redis list length)
kubectl apply -f examples/relay-transcription/keda-scaledobject.yaml
```

The KEDA ScaledObject targets `relay:<model>:pending` list length with `listLength: 1` and `minReplicaCount: 0` — pods scale to zero when the queue is empty. Replace all `<placeholder>` values (namespace, Redis address, secret names, image tags) before applying.
