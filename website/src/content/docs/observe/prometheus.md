---
title: Prometheus
---

# Prometheus

The gateway exposes a standard Prometheus metrics endpoint. The relay has no HTTP server and does not expose `/metrics` — its metrics are collected in-process and only relevant if you run a Prometheus push gateway or scrape the pod directly via annotations.

## Scrape endpoint

```
GET /metrics
```

Returns metrics in Prometheus text exposition format. No authentication required (secure at the network level).

## Enable with Helm

### ServiceMonitor

Requires [Prometheus Operator](https://github.com/prometheus-operator/prometheus-operator) in your cluster.

```yaml
metrics:
  serviceMonitor:
    enabled: true
    interval: 30s
    scrapeTimeout: 10s
    # Must match your Prometheus Operator's serviceMonitorSelector labels.
    # Example for kube-prometheus-stack:
    additionalLabels:
      release: kube-prometheus-stack
```

The ServiceMonitor is created in the same namespace as the Helm release by default. Override with `metrics.serviceMonitor.namespace` if your Prometheus Operator watches a different namespace.

### PrometheusRule

```yaml
alerting:
  prometheusRule:
    enabled: true
    # Must match your Prometheus Operator's ruleSelector labels.
    additionalLabels:
      release: kube-prometheus-stack
    # Configurable thresholds (defaults shown):
    thresholds:
      gatewayErrorRateWarning: 0.05    # 5%
      gatewayErrorRateCritical: 0.20   # 20%
      syncJobsInFlight: 20
      relayJobFailureRateWarning: 0.10  # 10%
      relayJobFailureRateCritical: 0.30 # 30%
      relayInferenceP95Seconds: 120
      rateLimitRejectionRateWarning: 0.20 # 20%
```

Add custom rules via `alerting.prometheusRule.extraRules` (appended to the `gatewai-relay` group).

## Consumer metrics

Two options for tracking per-consumer usage (mutually exclusive):

| Option | Helm key | When to use |
|--------|----------|-------------|
| Top-N gauge (Redis sorted set, refresh 60s) | `metricsConfig.topConsumers: 20` | Dynamic/large consumer pools |
| Direct labels on `gatewai_jobs_by_consumer_total` | `metricsConfig.consumerLabels: true` | ≤ 50 known consumers |

Avoid `consumerLabels: true` with large or dynamic consumer sets — it creates unbounded label cardinality.

## Dashboards

Four Grafana dashboards are available in [`dashboards/`](https://github.com/Qatr-io/GatewAI/tree/main/dashboards/):

| Dashboard | File | Datasource |
|-----------|------|------------|
| GatewAI — Gateway & Relay | `gatewai.json` | Prometheus + Tempo |
| GatewAI — LLM Proxy | `gatewai-llm.json` | Prometheus + Tempo |
| GatewAI — Audit Trail LLM | `gatewai-audit-trail.json` | Prometheus + Loki |
| GatewAI — Traces | `gatewai-traces.json` | Tempo |

Import via **Dashboards → Import → Upload JSON file**. The Prometheus dashboards include latency panels with [exemplar](/observe/opentelemetry#grafana--tempo-exemplars) support — clicking an exemplar dot jumps to the corresponding trace in Tempo.

For traces, OTLP metrics push (relay), and log forwarding see [OpenTelemetry](/observe/opentelemetry).

## Alerts

All alerts are defined in `helm/gateway/templates/prometheusrule.yaml`. Thresholds are configurable via `alerting.prometheusRule.thresholds`.

### Gateway

| Alert | Severity | Description | Threshold key |
|-------|----------|-------------|---------------|
| `KeventGatewayHighErrorRate` | warning | > X% of requests return 5xx over 5 min | `gatewayErrorRateWarning` |
| `KeventGatewayHighErrorRate` | critical | > X% of requests return 5xx over 5 min | `gatewayErrorRateCritical` |
| `KeventGatewayS3Errors` | warning | Any S3 errors in the last 5 min | — |
| `KeventGatewaySyncJobsInFlightHigh` | warning | Sync connections above threshold for 5 min | `syncJobsInFlight` |
| `KeventGatewayRateLimitHighRejectionRate` | warning | > X% of requests rate-limited per service type | `rateLimitRejectionRateWarning` |
| `KeventGatewayRateLimitErrors` | warning | Redis errors in rate limiter (fail-open) | — |

### Relay

| Alert | Severity | Description | Threshold key |
|-------|----------|-------------|---------------|
| `KeventRelayJobFailureRate` | warning | > X% of relay jobs failing per service type | `relayJobFailureRateWarning` |
| `KeventRelayJobFailureRate` | critical | > X% of relay jobs failing per service type | `relayJobFailureRateCritical` |
| `KeventRelayInferenceSlow` | warning | p95 inference latency above threshold for 10 min | `relayInferenceP95Seconds` |
| `KeventRelayS3Errors` | warning | Any relay S3 errors in the last 5 min | — |
