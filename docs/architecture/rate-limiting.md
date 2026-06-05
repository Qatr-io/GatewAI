# Rate limiting

The gateway enforces per-consumer, per-service fixed-window rate limits backed by Redis. Limits are keyed by `{consumer}:{service_type}:{user_type}` — models sharing the same `type` share the same counter.

## How it works

For each eligible request, the gateway runs an atomic Lua script in Redis:

```lua
local count = redis.call('INCR', key)
if count == 1 then redis.call('EXPIRE', key, window_seconds) end
return count
```

If `count > rate`, the gateway returns **`429 Too Many Requests`** with a `Retry-After` header (seconds until the window resets). The check is **fail-open**: Redis errors are logged and the request is allowed through.

Rate limiting applies at two points:
- **Async jobs** — before `ParseMultipartForm` in `POST /jobs/{service_type}`
- **Sync requests** — after path routing in `POST /v1/*`

## Configuration

```yaml
server:
  consumer_header: "X-Consumer-Username"   # identifies the consumer
  user_type_header: "X-User-Type"          # sa | user | premium | ...

rate_limits:
  audio:
    sa:
      rate: 100
      period: 1m
    user:
      rate: 20
      period: 1m
    "*":                    # fallback: applies when user_type is absent or unlisted
      rate: 10
      period: 1m
  ocr:
    "*":
      rate: 5
      period: 1m
```

### Unlimited user types

Set `rate: 0` to bypass Redis entirely for a specific user type. The `period` field is ignored when `rate` is `0`.

```yaml
rate_limits:
  audio:
    unlimited:
      rate: 0        # no limit — Redis is never touched for this user type
    sa:
      rate: 100
      period: 1m
    "*":
      rate: 10
      period: 1m
```

### Key concepts

- **`service_type`** — matches `services[].type` (e.g. `audio`, `ocr`, `llm`); all models of the same type share the counter.
- **`user_type`** — value from `server.user_type_header`; use `"*"` as a catch-all fallback.
- **`rate: 0`** — sentinel for "no limit"; the request is always allowed without incrementing any counter.
- **`period`** — accepts Go duration strings: `30s`, `1m`, `1h`, `24h`.
- **Absent consumer** — requests without a consumer header bypass rate limiting entirely (typically internal traffic).
- **Absent rate_limits entry** — no limit is applied for that `(service_type, user_type)` pair.

### Redis key format

```
rl:{consumer}:{service_type}:{user_type}
```

The key TTL is set to the window duration on first increment. It expires automatically when the window closes — no cleanup needed.

## Response

When a consumer exceeds their limit:

```http
HTTP/1.1 429 Too Many Requests
Content-Type: application/json
Retry-After: 42

{"error": {"message": "rate limit exceeded", "type": "http_429"}}
```

`Retry-After` is the number of seconds until the Redis key expires (i.e. until the window resets).

## Prometheus metrics

| Metric | Labels | Description |
|---|---|---|
| `GatewAI_ratelimit_requests_total` | `service_type, user_type, result` | All rate-limit checks (`allowed` / `rejected`) |
| `GatewAI_ratelimit_consumer_hits_total` | `service_type, user_type` | Consumers that hit their limit |
| `GatewAI_ratelimit_errors_total` | `service_type` | Redis errors during rate-limit check |

### Example queries

```promql
# Rejection rate per service (should stay below 20% in normal operation)
sum by (service_type) (rate(GatewAI_ratelimit_requests_total{result="rejected"}[5m]))
/
sum by (service_type) (rate(GatewAI_ratelimit_requests_total[5m]))

# Number of distinct consumers hitting their limit (user type = "user")
count(group by (consumer) (
  GatewAI_ratelimit_consumer_hits_total{user_type="user"}
))
```

## PrometheusRules

Two alerting rules are shipped with the Helm chart:

| Alert | Threshold | Severity |
|---|---|---|
| `KeventGatewayRateLimitHighRejectionRate` | > 20% rejections over 5 min | warning |
| `KeventGatewayRateLimitErrors` | any Redis error | warning |

See the runbooks: [KeventGatewayRateLimitHighRejectionRate](../runbooks/KeventGatewayRateLimitHighRejectionRate.md) and [KeventGatewayRateLimitErrors](../runbooks/KeventGatewayRateLimitErrors.md).
