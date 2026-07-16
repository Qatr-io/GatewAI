package usage

import (
	"context"
	"log/slog"
	"math"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// UsageTracker records per-consumer usage into permanent Redis sorted sets.
// All operations fail-open: Redis errors are logged and silently ignored.
type UsageTracker interface {
	TrackRequest(ctx context.Context, consumer, serviceType string)
	TrackJob(ctx context.Context, consumer, serviceType string)
	TrackProcessingTime(ctx context.Context, consumer, serviceType string, seconds float64)
	TrackTokens(ctx context.Context, consumer, serviceType string, prompt, completion int64)
	TrackActive(ctx context.Context, consumer string)
	// TrackUserType records the rate-limit tier a consumer was actually
	// evaluated under for a given service type, at the moment a real request
	// to that service was handled. A consumer can hold a different tier per
	// service (distinct roles per service upstream), so this is looked up
	// per service type rather than assumed from a single request's identity.
	TrackUserType(ctx context.Context, consumer, serviceType, userType string)
	UpdateRetention(d time.Duration)
}

// zincrWithTTL atomically increments a sorted-set member's score and sets a TTL
// on the key only on first creation (TTL == -1 means no expiry is currently set).
// Keys: KEYS[1]=key; Args: ARGV[1]=increment, ARGV[2]=member, ARGV[3]=ttlSeconds (0=no TTL)
var zincrWithTTL = redis.NewScript(`
local val = redis.call('ZINCRBY', KEYS[1], ARGV[1], ARGV[2])
if tonumber(ARGV[3]) > 0 and redis.call('TTL', KEYS[1]) == -1 then
    redis.call('EXPIRE', KEYS[1], ARGV[3])
end
return val
`)

// zaddGTWithTTL updates a member's score only when the new score is greater
// (used for last_active timestamps), and sets TTL on first creation.
// Keys: KEYS[1]=key; Args: ARGV[1]=score, ARGV[2]=member, ARGV[3]=ttlSeconds (0=no TTL)
var zaddGTWithTTL = redis.NewScript(`
redis.call('ZADD', KEYS[1], 'GT', ARGV[1], ARGV[2])
if tonumber(ARGV[3]) > 0 and redis.call('TTL', KEYS[1]) == -1 then
    redis.call('EXPIRE', KEYS[1], ARGV[3])
end
return 1
`)

// hsetWithTTL sets a hash field and sets a TTL on the key only on first
// creation (mirrors zincrWithTTL's TTL semantics for the hash case).
// Keys: KEYS[1]=key; Args: ARGV[1]=field, ARGV[2]=value, ARGV[3]=ttlSeconds (0=no TTL)
var hsetWithTTL = redis.NewScript(`
redis.call('HSET', KEYS[1], ARGV[1], ARGV[2])
if tonumber(ARGV[3]) > 0 and redis.call('TTL', KEYS[1]) == -1 then
    redis.call('EXPIRE', KEYS[1], ARGV[3])
end
return 1
`)

type redisUsageTracker struct {
	rdb       *redis.Client
	retention atomic.Int64 // nanoseconds; 0 = no TTL
}

// NewRedisUsageTracker returns a UsageTracker backed by Redis sorted sets.
func NewRedisUsageTracker(rdb *redis.Client, retention time.Duration) UsageTracker {
	t := &redisUsageTracker{rdb: rdb}
	t.retention.Store(int64(retention))
	return t
}

func (t *redisUsageTracker) ttlSeconds() int64 {
	d := time.Duration(t.retention.Load())
	if d <= 0 {
		return 0
	}
	return int64(math.Ceil(d.Seconds()))
}

func (t *redisUsageTracker) zincrBy(ctx context.Context, key string, delta float64, member string) {
	ttl := t.ttlSeconds()
	if err := zincrWithTTL.Run(ctx, t.rdb, []string{key}, delta, member, ttl).Err(); err != nil {
		slog.WarnContext(ctx, "usage tracker: zincrBy failed", "key", key, "error", err)
	}
}

func (t *redisUsageTracker) TrackRequest(ctx context.Context, consumer, serviceType string) {
	if consumer == "" {
		return
	}
	t.zincrBy(ctx, "usage:consumer:"+serviceType+":requests", 1, consumer)
}

func (t *redisUsageTracker) TrackJob(ctx context.Context, consumer, serviceType string) {
	if consumer == "" {
		return
	}
	t.zincrBy(ctx, "usage:consumer:"+serviceType+":jobs", 1, consumer)
}

func (t *redisUsageTracker) TrackProcessingTime(ctx context.Context, consumer, serviceType string, seconds float64) {
	if consumer == "" || seconds <= 0 {
		return
	}
	t.zincrBy(ctx, "usage:consumer:"+serviceType+":processing_time", seconds, consumer)
}

func (t *redisUsageTracker) TrackTokens(ctx context.Context, consumer, serviceType string, prompt, completion int64) {
	if consumer == "" || (prompt <= 0 && completion <= 0) {
		return
	}
	if prompt > 0 {
		t.zincrBy(ctx, "usage:consumer:"+serviceType+":tokens:prompt", float64(prompt), consumer)
	}
	if completion > 0 {
		t.zincrBy(ctx, "usage:consumer:"+serviceType+":tokens:completion", float64(completion), consumer)
	}
}

func (t *redisUsageTracker) TrackActive(ctx context.Context, consumer string) {
	if consumer == "" {
		return
	}
	now := float64(time.Now().Unix())
	ttl := t.ttlSeconds()
	if err := zaddGTWithTTL.Run(ctx, t.rdb, []string{"usage:consumers"}, now, consumer, ttl).Err(); err != nil {
		slog.WarnContext(ctx, "usage tracker: TrackActive failed", "consumer", consumer, "error", err)
	}
}

// TrackUserType records userType as consumer's current rate-limit tier for
// serviceType in a Redis hash ("usage:consumer:{serviceType}:usertype"),
// consulted by the usage store instead of trusting a single request's own
// resolved identity for every service type being reported.
func (t *redisUsageTracker) TrackUserType(ctx context.Context, consumer, serviceType, userType string) {
	if consumer == "" || userType == "" {
		return
	}
	key := "usage:consumer:" + serviceType + ":usertype"
	ttl := t.ttlSeconds()
	if err := hsetWithTTL.Run(ctx, t.rdb, []string{key}, consumer, userType, ttl).Err(); err != nil {
		slog.WarnContext(ctx, "usage tracker: TrackUserType failed", "key", key, "error", err)
	}
}

func (t *redisUsageTracker) UpdateRetention(d time.Duration) {
	t.retention.Store(int64(d))
}

// noopUsageTracker discards all calls.
type noopUsageTracker struct{}

func (noopUsageTracker) TrackRequest(_ context.Context, _, _ string)                   {}
func (noopUsageTracker) TrackJob(_ context.Context, _, _ string)                       {}
func (noopUsageTracker) TrackProcessingTime(_ context.Context, _, _ string, _ float64) {}
func (noopUsageTracker) TrackTokens(_ context.Context, _, _ string, _, _ int64)        {}
func (noopUsageTracker) TrackActive(_ context.Context, _ string)                       {}
func (noopUsageTracker) TrackUserType(_ context.Context, _, _, _ string)               {}
func (noopUsageTracker) UpdateRetention(_ time.Duration)                               {}

// NoopUsageTracker is a shared no-op tracker. Safe for concurrent use.
var NoopUsageTracker UsageTracker = noopUsageTracker{}
