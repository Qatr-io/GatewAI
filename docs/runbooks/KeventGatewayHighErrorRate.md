# KeventGatewayHighErrorRate

**Severity:** warning / critical
**Component:** Gateway

## Meaning

More than 5% (warning) or 20% (critical) of gateway requests are returning HTTP 5xx errors over the last 5 minutes.

## Impact

Clients are receiving errors. Async job submissions or sync inference calls are failing.

## Diagnosis

```bash
# Error rate by mode and service_type
rate(GatewAI_requests_total{status=~"5.."}[5m])

# Check gateway pod logs
kubectl logs -l app.kubernetes.io/name=gatewai-gateway -n <namespace> --tail=100

# Check which status codes are returned
sum by (status, mode, service_type) (
  rate(GatewAI_requests_total[5m])
)
```

Common causes:
- S3 unavailable → check `GatewAI_s3_errors_total`
- Redis unavailable → check `GatewAI_redis_errors_total`, gateway logs will show connection errors
- Upstream inference service returning errors (sync mode)

## Mitigation

1. Check S3 and Redis connectivity from the gateway pod
2. If S3 is down: async jobs will fail at upload — check bucket and credentials
3. If Redis is down: async submissions fail at enqueue and job state is lost — check Redis HA haproxy endpoint
4. Scale the gateway deployment if the issue is resource exhaustion
