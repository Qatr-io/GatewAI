---
title: Upgrade guide
---

# Upgrade guide

## Gateway

```bash
# 1. Pull the latest chart
helm repo update

# 2. Review the changelog for breaking changes
# https://github.com/Qatr-io/GatewAI/blob/main/CHANGELOG.md

# 3. Upgrade
helm upgrade gatewai-gateway GatewAI/gatewai-gateway -f values.yaml

# 4. Verify rollout
kubectl rollout status deployment/gatewai-gateway
kubectl logs -l app=gatewai-gateway --tail=50
```

## Relay

The relay image tag is set directly on the inference Deployment, not managed by the Helm chart.

```bash
# Update the relay container image tag in your Deployment manifest
# then apply:
kubectl set image deployment/whisper-large-v3 \
  relay=ghcr.io/qatr-io/gatewai/relay:<new-tag>

kubectl rollout status deployment/whisper-large-v3
```

Drain the queue before upgrading the relay if the new version changes the job payload format.

## Config changes

Config changes that don't require a restart can be applied via hot reload:

```bash
# Update the ConfigMap
kubectl edit configmap gatewai-gateway

# Trigger reload (if configmap-reload sidecar is not enabled)
kubectl exec deploy/gatewai-gateway -- \
  curl -s -X POST http://localhost:8080/-/reload
```

Enable `configReloader` in `values.yaml` to trigger reloads automatically on ConfigMap changes:

```yaml
configReloader:
  enabled: true
```

## Rollback

```bash
# Gateway
helm rollback gatewai-gateway

# Relay
kubectl rollout undo deployment/whisper-large-v3
```
