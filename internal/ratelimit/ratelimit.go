// Package ratelimit implements per-consumer, per-service-type rate limiting
// backed by Redis fixed-window counters (INCR + EXPIRE via Lua script).
//
// Rate limits are configured in config.yaml under the top-level rate_limits key,
// indexed by service type then user type:
//
//	rate_limits:
//	  audio:
//	    unlimited:
//	      rate: 0        # sentinel: no limit applied
//	    premium:
//	      rate: 100
//	      period: 1m
//	    "*":
//	      rate: 10
//	      period: 1m
//
// The user type is read from a configurable HTTP header (server.user_type_header),
// typically injected by OPA. "*" acts as fallback when the header is absent or
// the specific user type has no configured limit.
//
// Redis key format: rl:{consumer}:{service_type}:{user_type}
package ratelimit

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/redis/go-redis/v9"

	"gatewai/gateway/internal/config"
	"gatewai/gateway/internal/metrics"
)

// CheckResult holds the outcome of a rate-limit check together with the
// metadata needed to set X-RateLimit-* response headers.
type CheckResult struct {
	Allowed    bool
	Limit      int           // configured rate (0 = unlimited — headers not set)
	Remaining  int           // requests left in the current window; 0 when rejected
	ResetAfter time.Duration // time until the window resets; 0 when unlimited
}

// Checker is the interface implemented by Limiter.
// Handlers depend on this interface so they can be tested without Redis.
type Checker interface {
	// Check evaluates the rate limit for the given request and service type.
	// The returned CheckResult is valid even on error (Allowed=false, zero metadata).
	// On Redis errors callers should fail open.
	Check(ctx context.Context, r *http.Request, serviceType string) (CheckResult, error)
}

// Limiter checks rate limits using Redis fixed-window counters.
type Limiter struct {
	rdb            *redis.Client
	limits         map[string]map[string]config.RateLimitConfig
	consumerHeader string
	userTypeHeader string
}

// script atomically increments the counter, sets the TTL on first access,
// and returns {count, ttl} in all cases.
var script = redis.NewScript(`
local key   = KEYS[1]
local limit = tonumber(ARGV[1])
local ttl   = tonumber(ARGV[2])
local count = redis.call('INCR', key)
if count == 1 then
  redis.call('EXPIRE', key, ttl)
end
return {count, redis.call('TTL', key)}
`)

// New creates a Limiter. Pass the raw *redis.Client (available via storage.RedisClient.Client()).
func New(
	rdb *redis.Client,
	limits map[string]map[string]config.RateLimitConfig,
	consumerHeader, userTypeHeader string,
) *Limiter {
	return &Limiter{
		rdb:            rdb,
		limits:         limits,
		consumerHeader: consumerHeader,
		userTypeHeader: userTypeHeader,
	}
}

// Check evaluates the rate limit for the given request and service type.
func (l *Limiter) Check(ctx context.Context, r *http.Request, serviceType string) (CheckResult, error) {
	typeLimits, ok := l.limits[serviceType]
	if !ok {
		return CheckResult{Allowed: true}, nil
	}

	consumer := "anonymous"
	if l.consumerHeader != "" {
		if v := r.Header.Get(l.consumerHeader); v != "" {
			consumer = v
		}
	}

	userType := "*"
	if l.userTypeHeader != "" {
		if v := r.Header.Get(l.userTypeHeader); v != "" {
			userType = v
		}
	}

	// Exact match first, then "*" fallback.
	rlCfg, ok := typeLimits[userType]
	if !ok {
		rlCfg, ok = typeLimits["*"]
		if !ok {
			return CheckResult{Allowed: true}, nil
		}
	}

	// rate: 0 is the sentinel for "no limit" — allow without touching Redis.
	if rlCfg.Rate == 0 {
		metrics.RateLimitRequestsTotal.WithLabelValues(serviceType, userType, "allowed").Inc()
		return CheckResult{Allowed: true, Limit: 0}, nil
	}

	period, err := time.ParseDuration(rlCfg.Period)
	if err != nil {
		return CheckResult{}, fmt.Errorf("invalid rate_limit period %q for service %q: %w", rlCfg.Period, serviceType, err)
	}

	windowSec := int64(math.Ceil(period.Seconds()))
	key := fmt.Sprintf("rl:%s:%s:%s", consumer, serviceType, userType)

	vals, err := script.Run(ctx, l.rdb, []string{key}, rlCfg.Rate, windowSec).Int64Slice()
	if err != nil {
		metrics.RateLimitErrorsTotal.WithLabelValues(serviceType).Inc()
		return CheckResult{}, fmt.Errorf("rate limit script: %w", err)
	}

	count := vals[0]
	ttlSecs := vals[1]
	if ttlSecs < 0 {
		ttlSecs = windowSec
	}

	remaining := int(int64(rlCfg.Rate) - count)
	if remaining < 0 {
		remaining = 0
	}

	allowed := count <= int64(rlCfg.Rate)
	result := "allowed"
	if !allowed {
		result = "rejected"
	}
	metrics.RateLimitRequestsTotal.WithLabelValues(serviceType, userType, result).Inc()
	if consumer != "anonymous" {
		metrics.RateLimitConsumerHitsTotal.WithLabelValues(serviceType, userType, consumer).Inc()
	}

	return CheckResult{
		Allowed:    allowed,
		Limit:      rlCfg.Rate,
		Remaining:  remaining,
		ResetAfter: time.Duration(ttlSecs) * time.Second,
	}, nil
}
