// Package accounting performs the exactly-once, completion-time side effects
// that used to run on every gateway replica via broadcast Redis pub/sub:
// rate-limit budget debit and usage tracking. The relay is a true
// competing-consumer (one pod pops and processes exactly one job), so calling
// these once from PublishResult is correct by construction — unlike the
// gateway's onComplete callback, which every replica receives.
package accounting

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/redis/go-redis/v9"

	"gatewai/relay/internal/config"
)

// tokenAddScript increments the window counter and sets TTL on the first
// write only. Ported verbatim from the gateway's internal/ratelimit package —
// keep in sync if that script changes.
var tokenAddScript = redis.NewScript(`
local ttl   = tonumber(ARGV[1])
local delta = tonumber(ARGV[2])
local count = redis.call('INCRBY', KEYS[1], delta)
if count == delta then
    redis.call('EXPIRE', KEYS[1], ttl)
end
return count
`)

// Limiter performs completion-time rate-limit budget debits. It intentionally
// has no Check* methods — the gateway performs all submission-time gating;
// the relay only ever writes.
type Limiter struct {
	limits map[string]map[string]config.RateLimitConfig
}

// NewLimiter creates a Limiter from the relay's rate_limits config block.
// A nil or empty limits map makes every Add* call a no-op.
func NewLimiter(limits map[string]map[string]config.RateLimitConfig) *Limiter {
	return &Limiter{limits: limits}
}

func (l *Limiter) resolve(serviceType, userType string) (config.RateLimitConfig, bool) {
	byUserType, ok := l.limits[serviceType]
	if !ok {
		return config.RateLimitConfig{}, false
	}
	cfg, ok := byUserType[userType]
	if !ok {
		cfg, ok = byUserType["*"]
	}
	return cfg, ok
}

// AddTokens debits total tokens from consumer's token budget for serviceType.
// No-op if total <= 0 or no token_rate is configured for (serviceType, userType).
func (l *Limiter) AddTokens(ctx context.Context, rdb *redis.Client, consumer, userType, serviceType string, total int) error {
	if total <= 0 {
		return nil
	}
	cfg, ok := l.resolve(serviceType, userType)
	if !ok || cfg.TokenRate == 0 {
		return nil
	}
	period, err := time.ParseDuration(cfg.TokenPeriod)
	if err != nil {
		return fmt.Errorf("parse token_period %q: %w", cfg.TokenPeriod, err)
	}
	key := fmt.Sprintf("trl:%s:%s:%s", consumer, serviceType, userType)
	if err := tokenAddScript.Run(ctx, rdb, []string{key}, int(period.Seconds()), total).Err(); err != nil {
		slog.ErrorContext(ctx, "accounting: AddTokens failed", "key", key, "error", err)
	}
	return nil
}

// AddProcessingTime debits seconds (rounded up) from consumer's processing-time
// budget for serviceType. No-op if seconds <= 0 or no processing_time is
// configured for (serviceType, userType).
func (l *Limiter) AddProcessingTime(ctx context.Context, rdb *redis.Client, consumer, userType, serviceType string, seconds float64) error {
	if seconds <= 0 {
		return nil
	}
	cfg, ok := l.resolve(serviceType, userType)
	if !ok || cfg.ProcessingTime == 0 {
		return nil
	}
	period, err := time.ParseDuration(cfg.ProcessingPeriod)
	if err != nil {
		return fmt.Errorf("parse processing_period %q: %w", cfg.ProcessingPeriod, err)
	}
	key := fmt.Sprintf("ptrl:%s:%s:%s", consumer, serviceType, userType)
	delta := int(math.Ceil(seconds))
	if err := tokenAddScript.Run(ctx, rdb, []string{key}, int(period.Seconds()), delta).Err(); err != nil {
		slog.ErrorContext(ctx, "accounting: AddProcessingTime failed", "key", key, "error", err)
	}
	return nil
}
