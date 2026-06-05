# KeventGatewayRateLimitErrors

**Severity:** warning
**Component:** Gateway

## Meaning

The gateway is failing to execute rate-limit checks against Redis. The rate limiter is **fail-open**: Redis errors allow the request through, so consumers are not blocked — but rate limiting is not enforced.

## Impact

Rate limiting is silently disabled for the affected service type. Consumers can exceed their configured limits. This is a safety behaviour to avoid blocking traffic during infrastructure issues, but it should be resolved promptly.

## Diagnosis

```promql
# Error rate per service type
rate(GatewAI_ratelimit_errors_total[5m])

# Is Redis reachable?
# Check gateway pod logs for connection errors
kubectl logs -l app.kubernetes.io/name=gatewai-gateway -n <namespace> --tail=100 | grep "rate limit"
```

Common causes:
- Redis is unavailable or restarting (check Redis HA HAProxy)
- Redis connection pool exhausted under high load
- Network partition between gateway pods and Redis

## Mitigation

1. Check Redis HA status:
   ```bash
   kubectl get pods -l app=redis-ha -n <namespace>
   kubectl exec -it <haproxy-pod> -- redis-cli -h localhost -p 6380 PING
   ```

2. If Redis is down, other features are also affected (job records, cache, consumer tracking) — follow the Redis recovery procedure

3. Check gateway pod resource limits — if the pod is OOM-killing, Redis connections may be dropping

4. Once Redis recovers, the rate limiter resumes automatically (no restart needed)
