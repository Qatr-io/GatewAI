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

// ConcurrentChecker counts active (pending+processing) jobs per consumer
// on-the-fly from the Redis job store. No external counter to maintain.
type ConcurrentChecker interface {
	// CheckConcurrent reads the consumer's active job list and returns
	// Allowed=false when MaxConcurrent is reached. No-op (allowed) when
	// MaxConcurrent is 0 or no consumer header is configured. Fail-open on
	// Redis errors.
	CheckConcurrent(ctx context.Context, r *http.Request, serviceType string) (CheckResult, error)
}

// ProcessingTimeChecker checks and accumulates processing time budgets.
type ProcessingTimeChecker interface {
	// CheckProcessingTime checks whether the consumer has remaining processing
	// time budget. Optimistic: check before, add after. Fail-open on Redis errors.
	CheckProcessingTime(ctx context.Context, r *http.Request, serviceType string) (CheckResult, error)
	// AddProcessingTime records consumed seconds (ceiling rounded to int).
	// Sets TTL on first write. No-op when ProcessingTime is 0 or seconds <= 0.
	AddProcessingTime(ctx context.Context, consumer, userType, serviceType string, seconds float64) error
}

// TokenChecker is the interface for token-budget rate limiting implemented by Limiter.
type TokenChecker interface {
	// CheckTokens returns whether the caller still has token budget. Fail-open on Redis errors.
	CheckTokens(ctx context.Context, r *http.Request, serviceType string) (CheckResult, error)
	// AddTokens records total tokens consumed into the current window counter.
	AddTokens(ctx context.Context, r *http.Request, serviceType string, total int) error
	// CheckModelTokens returns whether the caller still has token budget for the given model.
	CheckModelTokens(ctx context.Context, r *http.Request, model string) (CheckResult, error)
	// AddModelTokens records total tokens consumed for the given model into the window counter.
	AddModelTokens(ctx context.Context, r *http.Request, model string, total int) error
}

// Limiter checks rate limits using Redis fixed-window counters.
type Limiter struct {
	rdb            *redis.Client
	limits         map[string]map[string]config.RateLimitConfig
	modelLimits    map[string]map[string]config.RateLimitConfig
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

// countActiveConcurrentScript iterates the consumer's job sorted set and counts
// pending+processing jobs for the given service_type.
// KEYS[1] = consumer:{name}:jobs (sorted set of job IDs)
// ARGV[1] = max_concurrent (int), ARGV[2] = service_type (string)
// Returns {count, 1} if count < max (allowed), {count, 0} otherwise (rejected).
var countActiveConcurrentScript = redis.NewScript(`
local ids = redis.call('ZRANGE', KEYS[1], 0, -1)
local count = 0
local svc = ARGV[2]
for _, id in ipairs(ids) do
    local data = redis.call('GET', 'job:' .. id)
    if data then
        local ok, job = pcall(cjson.decode, data)
        if ok and job then
            local s = job['status']
            if (s == 'pending' or s == 'processing') and job['service_type'] == svc then
                count = count + 1
            end
        end
    end
end
local max = tonumber(ARGV[1])
if count >= max then return {count, 0} end
return {count, 1}
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
// modelLimits maps model name → user type → RateLimitConfig for per-model token budgets;
// pass nil to disable model-level limiting.
func New(
	rdb *redis.Client,
	limits map[string]map[string]config.RateLimitConfig,
	modelLimits map[string]map[string]config.RateLimitConfig,
	consumerHeader, userTypeHeader string,
) *Limiter {
	return &Limiter{
		rdb:            rdb,
		limits:         limits,
		modelLimits:    modelLimits,
		consumerHeader: consumerHeader,
		userTypeHeader: userTypeHeader,
	}
}

// resolveFromMap resolves consumer, userType, and the matching RateLimitConfig
// for key in limitsMap. Falls back to "*" userType when the specific type is absent.
func (l *Limiter) resolveFromMap(r *http.Request, key string, limitsMap map[string]map[string]config.RateLimitConfig) (consumer, userType string, cfg config.RateLimitConfig) {
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
	keyLimits, ok := limitsMap[key]
	if !ok {
		return consumer, userType, config.RateLimitConfig{}
	}
	if c, ok := keyLimits[userType]; ok {
		return consumer, userType, c
	}
	if c, ok := keyLimits["*"]; ok {
		return consumer, "*", c
	}
	return consumer, userType, config.RateLimitConfig{}
}

// resolveIdentity returns the consumer ID, matched userType key, and the
// RateLimitConfig entry for serviceType. Falls back to "*" when the request's
// user-type header is absent or has no explicit entry.
func (l *Limiter) resolveIdentity(r *http.Request, serviceType string) (consumer, userType string, cfg config.RateLimitConfig) {
	return l.resolveFromMap(r, serviceType, l.limits)
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

// CheckModelTokens returns whether the caller still has token budget for the
// given model in the current window. No-op (allowed) when no model limits are
// configured. Fail-open on Redis errors.
func (l *Limiter) CheckModelTokens(ctx context.Context, r *http.Request, model string) (CheckResult, error) {
	consumer, userType, cfg := l.resolveFromMap(r, model, l.modelLimits)
	if cfg.TokenRate == 0 {
		return CheckResult{Allowed: true}, nil
	}

	period, err := time.ParseDuration(cfg.TokenPeriod)
	if err != nil {
		metrics.TokenRatelimitErrorsTotal.WithLabelValues("model:" + model).Inc()
		return CheckResult{Allowed: true}, fmt.Errorf("parse token_period %q: %w", cfg.TokenPeriod, err)
	}

	key := fmt.Sprintf("trl:%s:model:%s:%s", consumer, model, userType)

	raw, err := tokenCheckScript.Run(ctx, l.rdb, []string{key}, cfg.TokenRate).Slice()
	if err != nil {
		metrics.TokenRatelimitErrorsTotal.WithLabelValues("model:" + model).Inc()
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
	metrics.TokenRatelimitCheckedTotal.WithLabelValues("model:"+model, userType, result).Inc()

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

// AddModelTokens records total tokens consumed for the given model into the
// current window counter. No-op when no model limits are configured or total
// is 0. Fail-open on Redis errors.
func (l *Limiter) AddModelTokens(ctx context.Context, r *http.Request, model string, total int) error {
	if total == 0 {
		return nil
	}
	consumer, userType, cfg := l.resolveFromMap(r, model, l.modelLimits)
	if cfg.TokenRate == 0 {
		return nil
	}

	period, err := time.ParseDuration(cfg.TokenPeriod)
	if err != nil {
		return fmt.Errorf("parse token_period %q: %w", cfg.TokenPeriod, err)
	}

	key := fmt.Sprintf("trl:%s:model:%s:%s", consumer, model, userType)

	if err := tokenAddScript.Run(ctx, l.rdb, []string{key},
		int(period.Seconds()),
		total,
	).Err(); err != nil {
		metrics.TokenRatelimitErrorsTotal.WithLabelValues("model:" + model).Inc()
	}
	return nil
}

// CheckConcurrent implements ConcurrentChecker.
// It counts pending+processing jobs for the consumer from the Redis job store.
// Skips enforcement when no consumer can be identified (anonymous).
func (l *Limiter) CheckConcurrent(ctx context.Context, r *http.Request, serviceType string) (CheckResult, error) {
	consumer, userType, rlCfg := l.resolveIdentity(r, serviceType)

	if _, ok := l.limits[serviceType]; !ok {
		return CheckResult{Allowed: true}, nil
	}
	if rlCfg.MaxConcurrent == 0 {
		metrics.ConcurrentJobChecksTotal.WithLabelValues(serviceType, userType, "allowed").Inc()
		return CheckResult{Allowed: true}, nil
	}
	if consumer == "anonymous" {
		// No consumer header configured or header absent — cannot enforce per-consumer limit.
		return CheckResult{Allowed: true}, nil
	}

	key := fmt.Sprintf("consumer:%s:jobs", consumer)
	vals, err := countActiveConcurrentScript.Run(ctx, l.rdb, []string{key},
		rlCfg.MaxConcurrent, serviceType,
	).Int64Slice()
	if err != nil {
		metrics.RateLimitErrorsTotal.WithLabelValues(serviceType).Inc()
		return CheckResult{Allowed: true}, fmt.Errorf("concurrent count script: %w", err)
	}

	current := vals[0]
	allowed := vals[1] == 1
	remaining := int(int64(rlCfg.MaxConcurrent) - current)
	if remaining < 0 {
		remaining = 0
	}

	result := "allowed"
	if !allowed {
		result = "rejected"
	}
	metrics.ConcurrentJobChecksTotal.WithLabelValues(serviceType, userType, result).Inc()

	return CheckResult{
		Allowed:   allowed,
		Limit:     rlCfg.MaxConcurrent,
		Remaining: remaining,
	}, nil
}

// CheckProcessingTime implements ProcessingTimeChecker.
func (l *Limiter) CheckProcessingTime(ctx context.Context, r *http.Request, serviceType string) (CheckResult, error) {
	consumer, userType, cfg := l.resolveIdentity(r, serviceType)
	if cfg.ProcessingTime == 0 {
		return CheckResult{Allowed: true}, nil
	}

	period, err := time.ParseDuration(cfg.ProcessingPeriod)
	if err != nil {
		metrics.TokenRatelimitErrorsTotal.WithLabelValues("pt:" + serviceType).Inc()
		return CheckResult{Allowed: true}, fmt.Errorf("parse processing_period %q: %w", cfg.ProcessingPeriod, err)
	}

	key := fmt.Sprintf("ptrl:%s:%s:%s", consumer, serviceType, userType)
	raw, err := tokenCheckScript.Run(ctx, l.rdb, []string{key}, cfg.ProcessingTime).Slice()
	if err != nil {
		metrics.TokenRatelimitErrorsTotal.WithLabelValues("pt:" + serviceType).Inc()
		return CheckResult{Allowed: true}, nil // fail-open
	}

	count := raw[0].(int64)
	ttl := raw[1].(int64)
	allowed := count < int64(cfg.ProcessingTime)
	remaining := cfg.ProcessingTime - int(count)
	if remaining < 0 {
		remaining = 0
	}

	result := "allowed"
	if !allowed {
		result = "rejected"
	}
	metrics.ProcessingTimeChecksTotal.WithLabelValues(serviceType, userType, result).Inc()

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
		Limit:      cfg.ProcessingTime,
		Remaining:  remaining,
		ResetAfter: resetAfter,
	}, nil
}

// AddProcessingTime implements ProcessingTimeChecker.
func (l *Limiter) AddProcessingTime(ctx context.Context, consumer, userType, serviceType string, seconds float64) error {
	if seconds <= 0 {
		return nil
	}
	keyLimits, ok := l.limits[serviceType]
	if !ok {
		return nil
	}
	cfg, ok := keyLimits[userType]
	if !ok {
		cfg, ok = keyLimits["*"]
	}
	if !ok || cfg.ProcessingTime == 0 {
		return nil
	}

	period, err := time.ParseDuration(cfg.ProcessingPeriod)
	if err != nil {
		return fmt.Errorf("parse processing_period %q: %w", cfg.ProcessingPeriod, err)
	}

	key := fmt.Sprintf("ptrl:%s:%s:%s", consumer, serviceType, userType)
	delta := int(math.Ceil(seconds))
	if err := tokenAddScript.Run(ctx, l.rdb, []string{key},
		int(period.Seconds()),
		delta,
	).Err(); err != nil {
		metrics.TokenRatelimitErrorsTotal.WithLabelValues("pt:" + serviceType).Inc()
	}
	return nil
}
