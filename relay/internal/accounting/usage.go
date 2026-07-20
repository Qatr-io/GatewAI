package accounting

import (
	"context"
	"log/slog"
	"math"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// The following three scripts are ported verbatim from the gateway's
// internal/usage package — keep in sync if those change.

var zincrWithTTL = redis.NewScript(`
local val = redis.call('ZINCRBY', KEYS[1], ARGV[1], ARGV[2])
if tonumber(ARGV[3]) > 0 and redis.call('TTL', KEYS[1]) == -1 then
    redis.call('EXPIRE', KEYS[1], ARGV[3])
end
return val
`)

var zaddGTWithTTL = redis.NewScript(`
redis.call('ZADD', KEYS[1], 'GT', ARGV[1], ARGV[2])
if tonumber(ARGV[3]) > 0 and redis.call('TTL', KEYS[1]) == -1 then
    redis.call('EXPIRE', KEYS[1], ARGV[3])
end
return 1
`)

var hsetWithTTL = redis.NewScript(`
redis.call('HSET', KEYS[1], ARGV[1], ARGV[2])
if tonumber(ARGV[3]) > 0 and redis.call('TTL', KEYS[1]) == -1 then
    redis.call('EXPIRE', KEYS[1], ARGV[3])
end
return 1
`)

// Tracker records per-consumer usage into the same Redis sorted sets/hashes
// the gateway's usage.UsageTracker writes at submission time (TrackRequest,
// TrackJob — unaffected by this migration). Tracker only covers the
// completion-time fields, which only the relay can report exactly once.
type Tracker struct {
	rdb       *redis.Client
	retention atomic.Int64 // nanoseconds; 0 = no TTL
}

// NewTracker returns a Tracker backed by rdb with the given key retention.
func NewTracker(rdb *redis.Client, retention time.Duration) *Tracker {
	t := &Tracker{rdb: rdb}
	t.retention.Store(int64(retention))
	return t
}

func (t *Tracker) ttlSeconds() int64 {
	d := time.Duration(t.retention.Load())
	if d <= 0 {
		return 0
	}
	return int64(math.Ceil(d.Seconds()))
}

func (t *Tracker) zincrBy(ctx context.Context, key string, delta float64, member string) {
	ttl := t.ttlSeconds()
	if err := zincrWithTTL.Run(ctx, t.rdb, []string{key}, delta, member, ttl).Err(); err != nil {
		slog.WarnContext(ctx, "accounting: usage zincrBy failed", "key", key, "error", err)
	}
}

// TrackProcessingTime accumulates seconds into usage:consumer:{serviceType}:processing_time.
func (t *Tracker) TrackProcessingTime(ctx context.Context, consumer, serviceType string, seconds float64) {
	if consumer == "" || seconds <= 0 {
		return
	}
	t.zincrBy(ctx, "usage:consumer:"+serviceType+":processing_time", seconds, consumer)
}

// TrackTokens accumulates prompt/completion tokens into their respective keys.
func (t *Tracker) TrackTokens(ctx context.Context, consumer, serviceType string, prompt, completion int64) {
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

// TrackActive records consumer's last-active timestamp (monotonic max) in usage:consumers.
func (t *Tracker) TrackActive(ctx context.Context, consumer string) {
	if consumer == "" {
		return
	}
	now := float64(time.Now().Unix())
	ttl := t.ttlSeconds()
	if err := zaddGTWithTTL.Run(ctx, t.rdb, []string{"usage:consumers"}, now, consumer, ttl).Err(); err != nil {
		slog.WarnContext(ctx, "accounting: TrackActive failed", "consumer", consumer, "error", err)
	}
}

// TrackUserType records the rate-limit tier consumer was evaluated under for
// serviceType, in usage:consumer:{serviceType}:usertype.
func (t *Tracker) TrackUserType(ctx context.Context, consumer, serviceType, userType string) {
	if consumer == "" || userType == "" {
		return
	}
	key := "usage:consumer:" + serviceType + ":usertype"
	ttl := t.ttlSeconds()
	if err := hsetWithTTL.Run(ctx, t.rdb, []string{key}, consumer, userType, ttl).Err(); err != nil {
		slog.WarnContext(ctx, "accounting: TrackUserType failed", "key", key, "error", err)
	}
}
