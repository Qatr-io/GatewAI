// Package concurrency provides per-model concurrency limiting for sync inference calls.
package concurrency

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"

	"gatewai/gateway/internal/service"
)

// semaphoreTTL is the Redis key lifetime — a safety net that resets the counter
// if a gateway replica crashes while holding a slot.
const semaphoreTTL = 30 * time.Minute

// acquireScript atomically increments the counter and checks the limit.
// Decrements and returns 0 if the limit is exceeded; returns 1 on success.
// The TTL is refreshed on every acquire so the key stays alive while slots are held.
var acquireScript = redis.NewScript(`
local key  = KEYS[1]
local max  = tonumber(ARGV[1])
local ttl  = tonumber(ARGV[2])
local cur  = redis.call("INCR", key)
redis.call("EXPIRE", key, ttl)
if cur > max then
    redis.call("DECR", key)
    return 0
end
return 1
`)

// releaseScript decrements the counter and floors it at 0 to guard against
// stale decrements after a crash recovery.
var releaseScript = redis.NewScript(`
local key = KEYS[1]
local val = redis.call("DECR", key)
if val < 0 then
    redis.call("SET", key, "0")
end
return val
`)

// ModelSemaphore limits the number of simultaneous sync proxy calls per model
// using a shared Redis counter so the limit is enforced across all gateway replicas.
// Only models with MaxConcurrentSync > 0 are tracked; others are unconstrained.
type ModelSemaphore struct {
	rdb    *redis.Client
	limits map[string]int // model → max concurrent sync calls
}

// NewModelSemaphore builds a ModelSemaphore from the registry.
// Returns nil when no model in the registry has a sync limit configured.
func NewModelSemaphore(reg *service.Registry, rdb *redis.Client) *ModelSemaphore {
	limits := make(map[string]int)
	for _, def := range reg.All() {
		if def.MaxConcurrentSync > 0 {
			limits[def.Model] = def.MaxConcurrentSync
		}
	}
	if len(limits) == 0 {
		return nil
	}
	return &ModelSemaphore{rdb: rdb, limits: limits}
}

// TryAcquire attempts to acquire a sync slot for the given model.
// Returns true (slot taken) or false (all slots busy → caller should return 503).
// On Redis error, fails open (returns true) to avoid cascading failures.
func (s *ModelSemaphore) TryAcquire(model string) bool {
	max, ok := s.limits[model]
	if !ok {
		return true
	}
	ttlSecs := int(semaphoreTTL.Seconds())
	res, err := acquireScript.Run(context.Background(), s.rdb,
		[]string{"gateway:semaphore:sync:" + model},
		max, ttlSecs,
	).Int()
	if err != nil {
		return true // fail open
	}
	return res == 1
}

// Release returns a sync slot for the given model after a request completes.
// No-op when model has no limit configured.
func (s *ModelSemaphore) Release(model string) {
	if _, ok := s.limits[model]; !ok {
		return
	}
	_ = releaseScript.Run(context.Background(), s.rdb,
		[]string{"gateway:semaphore:sync:" + model},
	).Err()
}
