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
//
// Policy-level limits (per-member, from the matched authz rule) are layered on
// top via WithPolicyLimits / policyLimitsFromContext. Keys use the prefix
// "rlp:" (request rate) and "trlp:" (token budget) keyed per consumer:
//
//	rlp:{consumer}:{service_type}
//	trlp:{consumer}:{service_type}
//
// Anonymous consumers (no consumer header) are always allowed by the policy
// layer (cannot enforce per-member limits without a stable identity).
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

// policyLimitsKey is the unexported context key type for policy limits.
type policyLimitsKey struct{}

// WithPolicyLimits attaches the matched policy rule's RateLimitConfig to ctx.
// Pass nil to clear any previously stored value (treated as no policy limits).
func WithPolicyLimits(ctx context.Context, cfg *config.RateLimitConfig) context.Context {
	return context.WithValue(ctx, policyLimitsKey{}, cfg)
}

// policyLimitsFromContext retrieves the RateLimitConfig stored by
// WithPolicyLimits. Returns nil when no value is present.
func policyLimitsFromContext(ctx context.Context) *config.RateLimitConfig {
	v, _ := ctx.Value(policyLimitsKey{}).(*config.RateLimitConfig)
	return v
}

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
// on-the-fly from the Redis job store, combined with a short-lived in-flight
// slot to prevent TOCTOU races under concurrent submissions.
type ConcurrentChecker interface {
	// CheckConcurrent reads the consumer's active job list and returns
	// Allowed=false when MaxConcurrent is reached. When allowed, it atomically
	// increments a short-lived in-flight counter so concurrent submitters see
	// each other's pending slot. Fail-open on Redis errors.
	CheckConcurrent(ctx context.Context, r *http.Request, serviceType string) (CheckResult, error)
	// ReleaseSlot decrements the in-flight counter after SaveJob completes
	// (success or failure). Must be called exactly once per allowed CheckConcurrent.
	ReleaseSlot(ctx context.Context, r *http.Request, serviceType string) error
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

// checkAndReserveScript counts active jobs from the consumer sorted set plus the
// in-flight counter, then atomically reserves a slot if the total is below max.
// This prevents TOCTOU races: concurrent submitters see each other's pending slot
// before their job is persisted.
// KEYS[1] = consumer:{name}:jobs (sorted set of job IDs)
// KEYS[2] = jc:{consumer}:{service}:{userType} (in-flight counter)
// ARGV[1] = max_concurrent (int), ARGV[2] = service_type (string),
// ARGV[3] = slot_ttl (seconds — safety net for pod crashes)
// Returns {total, 1} if slot reserved (allowed), {total, 0} if rejected.
var checkAndReserveScript = redis.NewScript(`
local ids = redis.call('ZRANGE', KEYS[1], 0, -1)
local active = 0
local svc = ARGV[2]
for _, id in ipairs(ids) do
    local data = redis.call('GET', 'job:' .. id)
    if data then
        local ok, job = pcall(cjson.decode, data)
        if ok and job then
            local s = job['status']
            if (s == 'pending' or s == 'processing') and job['service_type'] == svc then
                active = active + 1
            end
        end
    end
end
local inflight = tonumber(redis.call('GET', KEYS[2]) or '0')
local total = active + inflight
local max = tonumber(ARGV[1])
if total >= max then return {total, 0} end
local new = redis.call('INCR', KEYS[2])
redis.call('EXPIRE', KEYS[2], tonumber(ARGV[3]))
return {new + active, 1}
`)

// releaseSlotScript decrements the in-flight counter with a floor of 0.
// KEYS[1] = jc:{consumer}:{service}:{userType}
var releaseSlotScript = redis.NewScript(`
local count = tonumber(redis.call('GET', KEYS[1]) or '0')
if count <= 0 then return 0 end
return redis.call('DECR', KEYS[1])
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
// Both the service-level rate_limits AND any policy limits from ctx must pass.
func (l *Limiter) Check(ctx context.Context, r *http.Request, serviceType string) (CheckResult, error) {
	consumer, userType, rlCfg := l.resolveIdentity(r, serviceType)

	// svcResult holds the outcome of the service-level check (may be allowed/no-op
	// when no rate_limits are configured for this service type).
	var svcResult CheckResult

	if _, ok := l.limits[serviceType]; !ok {
		// No config for this service type → service check always allowed.
		svcResult = CheckResult{Allowed: true}
	} else if rlCfg.Rate == 0 {
		// rate: 0 is the sentinel for "no limit" — allow without touching Redis.
		metrics.RateLimitRequestsTotal.WithLabelValues(serviceType, userType, "allowed").Inc()
		svcResult = CheckResult{Allowed: true, Limit: 0}
	} else {
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

		svcResult = CheckResult{
			Allowed:    allowed,
			Limit:      rlCfg.Rate,
			Remaining:  remaining,
			ResetAfter: time.Duration(ttlSecs) * time.Second,
		}
	}

	// If the service-level check already rejected, return immediately.
	if !svcResult.Allowed {
		return svcResult, nil
	}

	// Policy-level check: per-consumer per-service-type fixed window.
	// Skipped for anonymous consumers (no stable identity to key by).
	policyCfg := policyLimitsFromContext(ctx)
	if policyCfg == nil || policyCfg.Rate == 0 || consumer == "anonymous" {
		return svcResult, nil
	}

	policyPeriod, err := time.ParseDuration(policyCfg.Period)
	if err != nil {
		// Fail open: bad period string → skip policy check.
		return svcResult, nil
	}

	windowSec := int64(math.Ceil(policyPeriod.Seconds()))
	policyKey := fmt.Sprintf("rlp:%s:%s", consumer, serviceType)

	vals, err := script.Run(ctx, l.rdb, []string{policyKey}, policyCfg.Rate, windowSec).Int64Slice()
	if err != nil {
		metrics.RateLimitErrorsTotal.WithLabelValues(serviceType).Inc()
		// Fail open on Redis error.
		return svcResult, nil
	}

	count := vals[0]
	ttlSecs := vals[1]
	if ttlSecs < 0 {
		ttlSecs = windowSec
	}

	remaining := int(int64(policyCfg.Rate) - count)
	if remaining < 0 {
		remaining = 0
	}

	policyAllowed := count <= int64(policyCfg.Rate)
	if !policyAllowed {
		return CheckResult{
			Allowed:    false,
			Limit:      policyCfg.Rate,
			Remaining:  0,
			ResetAfter: time.Duration(ttlSecs) * time.Second,
		}, nil
	}

	return svcResult, nil
}

// CheckTokens returns whether the caller still has token budget in the current
// window. Allowed is always true when TokenRate is 0 (disabled). Fail-open on
// Redis errors. Additionally enforces any policy-level token budget from ctx.
func (l *Limiter) CheckTokens(ctx context.Context, r *http.Request, serviceType string) (CheckResult, error) {
	consumer, userType, cfg := l.resolveIdentity(r, serviceType)

	var svcResult CheckResult
	if cfg.TokenRate == 0 {
		svcResult = CheckResult{Allowed: true}
	} else {
		period, err := time.ParseDuration(cfg.TokenPeriod)
		if err != nil {
			metrics.TokenRatelimitErrorsTotal.WithLabelValues(serviceType).Inc()
			return CheckResult{Allowed: true}, fmt.Errorf("parse token_period %q: %w", cfg.TokenPeriod, err)
		}

		key := fmt.Sprintf("trl:%s:%s:%s", consumer, serviceType, userType)

		raw, err := tokenCheckScript.Run(ctx, l.rdb, []string{key}, cfg.TokenRate).Slice()
		if err != nil {
			metrics.TokenRatelimitErrorsTotal.WithLabelValues(serviceType).Inc()
			svcResult = CheckResult{Allowed: true} // fail-open
		} else {
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

			svcResult = CheckResult{
				Allowed:    allowed,
				Limit:      cfg.TokenRate,
				Remaining:  remaining,
				ResetAfter: resetAfter,
			}
		}
	}

	// Return immediately if service-level check rejected.
	if !svcResult.Allowed {
		return svcResult, nil
	}

	// Policy-level token check: per-consumer per-service-type.
	// Skipped for anonymous consumers.
	policyCfg := policyLimitsFromContext(ctx)
	if policyCfg == nil || policyCfg.TokenRate == 0 || consumer == "anonymous" {
		return svcResult, nil
	}

	policyPeriod, err := time.ParseDuration(policyCfg.TokenPeriod)
	if err != nil {
		// Fail open: bad period string → skip policy token check.
		return svcResult, nil
	}

	policyKey := fmt.Sprintf("trlp:%s:%s", consumer, serviceType)
	raw, err := tokenCheckScript.Run(ctx, l.rdb, []string{policyKey}, policyCfg.TokenRate).Slice()
	if err != nil {
		metrics.TokenRatelimitErrorsTotal.WithLabelValues(serviceType).Inc()
		return svcResult, nil // fail-open
	}

	count := raw[0].(int64)
	ttl := raw[1].(int64)

	policyAllowed := count < int64(policyCfg.TokenRate)
	if !policyAllowed {
		remaining := 0
		var resetAfter time.Duration
		if ttl > 0 {
			resetAfter = time.Duration(ttl) * time.Second
		} else {
			resetAfter = policyPeriod
		}
		return CheckResult{
			Allowed:    false,
			Limit:      policyCfg.TokenRate,
			Remaining:  remaining,
			ResetAfter: resetAfter,
		}, nil
	}

	return svcResult, nil
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
// Redis errors. Additionally updates the policy-level token counter when
// policy limits are present in ctx.
func (l *Limiter) AddTokens(ctx context.Context, r *http.Request, serviceType string, total int) error {
	if total == 0 {
		return nil
	}
	consumer, userType, cfg := l.resolveIdentity(r, serviceType)
	if cfg.TokenRate > 0 {
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
	}

	// Policy-level token accumulation: per-consumer per-service-type.
	// Skipped for anonymous consumers.
	policyCfg := policyLimitsFromContext(ctx)
	if policyCfg != nil && policyCfg.TokenRate > 0 && consumer != "anonymous" {
		policyPeriod, err := time.ParseDuration(policyCfg.TokenPeriod)
		if err == nil {
			policyKey := fmt.Sprintf("trlp:%s:%s", consumer, serviceType)
			if err := tokenAddScript.Run(ctx, l.rdb, []string{policyKey},
				int(policyPeriod.Seconds()),
				total,
			).Err(); err != nil {
				metrics.TokenRatelimitErrorsTotal.WithLabelValues(serviceType).Inc()
			}
		}
		// Fail open: bad TokenPeriod → skip policy token accumulation.
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

// concurrentSlotTTL is the TTL applied to the in-flight slot counter as a
// safety net in case the gateway crashes between CheckConcurrent and ReleaseSlot.
// Normal usage always calls ReleaseSlot within milliseconds.
const concurrentSlotTTL = 300 // 5 minutes

// CheckConcurrent implements ConcurrentChecker.
// Counts pending+processing jobs from the sorted set plus any in-flight slots
// from concurrent submitters. Atomically reserves a slot when allowed.
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
		// No consumer header configured or absent — cannot enforce per-consumer limit.
		return CheckResult{Allowed: true}, nil
	}

	consumerJobsKey := fmt.Sprintf("consumer:%s:jobs", consumer)
	inflightKey := fmt.Sprintf("jc:%s:%s:%s", consumer, serviceType, userType)
	vals, err := checkAndReserveScript.Run(ctx, l.rdb,
		[]string{consumerJobsKey, inflightKey},
		rlCfg.MaxConcurrent, serviceType, concurrentSlotTTL,
	).Int64Slice()
	if err != nil {
		metrics.RateLimitErrorsTotal.WithLabelValues(serviceType).Inc()
		return CheckResult{Allowed: true}, fmt.Errorf("concurrent check script: %w", err)
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

// ReleaseSlot implements ConcurrentChecker.
// Decrements the in-flight counter after SaveJob completes.
func (l *Limiter) ReleaseSlot(ctx context.Context, r *http.Request, serviceType string) error {
	consumer, userType, rlCfg := l.resolveIdentity(r, serviceType)
	if rlCfg.MaxConcurrent == 0 || consumer == "anonymous" {
		return nil
	}
	key := fmt.Sprintf("jc:%s:%s:%s", consumer, serviceType, userType)
	if err := releaseSlotScript.Run(ctx, l.rdb, []string{key}).Err(); err != nil {
		metrics.RateLimitErrorsTotal.WithLabelValues(serviceType).Inc()
		return fmt.Errorf("release slot script: %w", err)
	}
	return nil
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
