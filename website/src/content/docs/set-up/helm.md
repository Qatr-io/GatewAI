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

Apply the manifests from the repository:

```bash
# ConfigMap (relay config.yaml)
kubectl apply -f k8s/relay-cm.yaml

# Deployment (2 containers: inference model + relay)
kubectl apply -f k8s/deployment-transcription.yaml

# KEDA ScaledObject (scales on Redis list length)
kubectl apply -f k8s/keda-transcription.yaml
```

The KEDA ScaledObject targets `relay:<model>:pending` list length with `listLength: 1` and `minReplicaCount: 0` — pods scale to zero when the queue is empty. Replace the `<placeholder>` values (Redis address, secret names, image tags) before applying.
