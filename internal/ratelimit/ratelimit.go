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

// tokenCheckScript reads the current window token count.
// Returns {count, TTL} — TTL is -1 when the budget is not exhausted.
var tokenCheckScript = redis.NewScript(`
local count = tonumber(redis.call('GET', KEYS[1]) or '0')
local limit  = tonumber(ARGV[1])
if count >= limit then
    return {count, redis.call('TTL', KEYS[1])}
end
return {count, -1}
`)

// tokenAddScript increments the window counter and sets TTL on the first write.
var tokenAddScript = redis.NewScript(`
local ttl   = tonumber(ARGV[1])
local delta = tonumber(ARGV[2])
local count = redis.call('INCRBY', KEYS[1], delta)
if count == delta then
    redis.call('EXPIRE', KEYS[1], ttl)
end
return count
`)

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

// resolveIdentity returns the consumer ID, matched userType key, and the
// RateLimitConfig entry for serviceType. Falls back to "*" when the request's
// user-type header is absent or has no explicit entry.
func (l *Limiter) resolveIdentity(r *http.Request, serviceType string) (consumer, userType string, cfg config.RateLimitConfig) {
	consumer = "anonymous"
	if l.consumerHeader != "" {
		if v := r.Header.Get(l.consumerHeader); v != "" {
			consumer = v
		}
	}

	userType = "*"
	if l.userTypeHeader != "" {
		if v := r.Header.Get(l.userTypeHeader); v != "" {
			userType = v
		}
	}

	svcLimits, ok := l.limits[serviceType]
	if !ok {
		return consumer, userType, config.RateLimitConfig{}
	}
	if c, ok := svcLimits[userType]; ok {
		return consumer, userType, c
	}
	if c, ok := svcLimits["*"]; ok {
		return consumer, "*", c
	}
	return consumer, userType, config.RateLimitConfig{}
}

// Check evaluates the rate limit for the given request and service type.
func (l *Limiter) Check(ctx context.Context, r *http.Request, serviceType string) (CheckResult, error) {
	consumer, userType, rlCfg := l.resolveIdentity(r, serviceType)

	// No config for this service type → always allowed.
	if _, ok := l.limits[serviceType]; !ok {
		return CheckResult{Allowed: true}, nil
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

// CheckTokens returns whether the caller still has token budget in the current
// window. Allowed is always true when TokenRate is 0 (disabled). Fail-open on
// Redis errors.
func (l *Limiter) CheckTokens(ctx context.Context, r *http.Request, serviceType string) (CheckResult, error) {
	consumer, userType, cfg := l.resolveIdentity(r, serviceType)
	if cfg.TokenRate == 0 {
		return CheckResult{Allowed: true}, nil
	}

	period, err := time.ParseDuration(cfg.TokenPeriod)
	if err != nil {
		metrics.TokenRatelimitErrorsTotal.WithLabelValues(serviceType).Inc()
		return CheckResult{Allowed: true}, fmt.Errorf("parse token_period %q: %w", cfg.TokenPeriod, err)
	}

	key := fmt.Sprintf("trl:%s:%s:%s", consumer, serviceType, userType)

	raw, err := tokenCheckScript.Run(ctx, l.rdb, []string{key}, cfg.TokenRate).Slice()
	if err != nil {
		metrics.TokenRatelimitErrorsTotal.WithLabelValues(serviceType).Inc()
		return CheckResult{Allowed: true}, nil // fail-open
	}

	count := raw[0].(int64)
	ttl := raw[1].(int64)

	allowed := count < int64(cfg.TokenRate)
	remaining := cfg.TokenRate - int(count)
	if remaining < 0 {
		remaining = 0
	}

	result := "allowed"
	if !allowed {
		result = "rejected"
	}
	metrics.TokenRatelimitCheckedTotal.WithLabelValues(serviceType, userType, result).Inc()

	var resetAfter time.Duration
	if !allowed {
		if ttl > 0 {
			resetAfter = time.Duration(ttl) * time.Second
		} else {
			resetAfter = period
		}
	}

	return CheckResult{
		Allowed:    allowed,
		Limit:      cfg.TokenRate,
		Remaining:  remaining,
		ResetAfter: resetAfter,
	}, nil
}

// AddTokens records total tokens consumed by the current request into the
// window counter. No-op when TokenRate is 0 or total is 0. Fail-open on
// Redis errors.
func (l *Limiter) AddTokens(ctx context.Context, r *http.Request, serviceType string, total int) error {
	if total == 0 {
		return nil
	}
	consumer, userType, cfg := l.resolveIdentity(r, serviceType)
	if cfg.TokenRate == 0 {
		return nil
	}

	period, err := time.ParseDuration(cfg.TokenPeriod)
	if err != nil {
		return fmt.Errorf("parse token_period %q: %w", cfg.TokenPeriod, err)
	}

	key := fmt.Sprintf("trl:%s:%s:%s", consumer, serviceType, userType)

	if err := tokenAddScript.Run(ctx, l.rdb, []string{key},
		int(period.Seconds()),
		total,
	).Err(); err != nil {
		metrics.TokenRatelimitErrorsTotal.WithLabelValues(serviceType).Inc()
	}
	return nil
}
