# KeventGatewayRateLimitHighRejectionRate

**Severity:** warning
**Component:** Gateway

## Meaning

More than 20% of rate-limit checks are returning `rejected` over the last 5 minutes. This means a significant fraction of API consumers are hitting their configured rate limits.

## Impact

Affected consumers receive `429 Too Many Requests` with a `Retry-After` header. Legitimate traffic may be disrupted if limits are set too low or if there is a sudden burst from a legitimate consumer.

## Diagnosis

```promql
# Rejection rate per service type
sum by (service_type) (rate(GatewAI_ratelimit_requests_total{result="rejected"}[5m]))
/
sum by (service_type) (rate(GatewAI_ratelimit_requests_total[5m]))

# Which user types are hitting limits?
sum by (service_type, user_type) (rate(GatewAI_ratelimit_consumer_hits_total[5m]))

# Total rejection volume
rate(GatewAI_ratelimit_requests_total{result="rejected"}[5m])
```

Common causes:
- A legitimate consumer is experiencing a traffic spike (burst usage)
- Rate limits are configured too low for current usage patterns
- A misconfigured client is retrying aggressively
- Abuse / unexpected load from a specific consumer

## Mitigation

1. **Identify the offending consumers** — query Redis directly for the top consumers hitting limits:
   ```bash
   redis-cli ZREVRANGEBYSCORE llm:consumer:tokens:sa:prompt +inf -inf WITHSCORES LIMIT 0 10
   ```

2. **Check if limits are appropriate** — compare current rates against configured limits in `config.yaml` under `rate_limits`

3. **Temporarily raise limits** if legitimate consumers are being disrupted — update `config.yaml` and `POST /-/reload`

4. **If abuse is detected** — disable the consumer at the APISIX level (revoke API key or credential)

5. **For sustained bursts** — consider increasing limits for the affected `user_type` or adding a dedicated tier
