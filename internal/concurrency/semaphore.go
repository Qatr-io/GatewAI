// Package concurrency provides per-model concurrency limiting for sync inference calls.
package concurrency

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"

	"gatewai/gateway/internal/config"
	"gatewai/gateway/internal/service"
)

// semaphoreTTL is the Redis key lifetime — a safety net that resets the counter
// if a gateway replica crashes while holding a slot.
const semaphoreTTL = 30 * time.Minute

// acquireScript atomically tries the reserved pool (when isPriority and a
// reservation is configured) before falling back to the shared pool.
// Returns {acquired, usedReserved} as 0/1 pairs.
// The TTL is refreshed on every acquire so keys stay alive while slots are held.
var acquireScript = redis.NewScript(`
local reservedKey = KEYS[1]
local sharedKey    = KEYS[2]
local reservedMax  = tonumber(ARGV[1])
local sharedMax    = tonumber(ARGV[2])
local ttl          = tonumber(ARGV[3])
local isPriority   = tonumber(ARGV[4])

if isPriority == 1 and reservedMax > 0 then
    local r = redis.call("INCR", reservedKey)
    redis.call("EXPIRE", reservedKey, ttl)
    if r <= reservedMax then
        return {1, 1}
    end
    redis.call("DECR", reservedKey)
end

local s = redis.call("INCR", sharedKey)
redis.call("EXPIRE", sharedKey, ttl)
if s <= sharedMax then
    return {1, 0}
end
redis.call("DECR", sharedKey)
return {0, 0}
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
// using shared Redis counters so the limit is enforced across all gateway replicas.
// Only models with MaxConcurrentSync > 0 are tracked; others are unconstrained.
// A model may additionally reserve part of its pool exclusively for requests
// carrying server.priority_header (see PriorityReservedSync).
type ModelSemaphore struct {
	rdb      *redis.Client
	limits   map[string]int // key (model or pool name) → shared pool max
	reserved map[string]int // key → reserved pool max (priority only); absent/0 = no reservation
	// keyPrefix namespaces the Redis keys. Empty (default) reproduces today's
	// exact "gateway:semaphore:sync:{model}" format byte-for-byte — no
	// key-migration risk on deploy. NewPoolSemaphore sets "pool:" so a pool
	// name colliding with a model name can never cross-contaminate.
	keyPrefix string
}

// NewModelSemaphore builds a ModelSemaphore from the registry.
// Returns nil when no model in the registry has a sync limit configured.
func NewModelSemaphore(reg *service.Registry, rdb *redis.Client) *ModelSemaphore {
	limits := make(map[string]int)
	reserved := make(map[string]int)
	for _, def := range reg.All() {
		if def.MaxConcurrentSync > 0 {
			r := def.PriorityReservedSync
			if r < 0 || r > def.MaxConcurrentSync {
				r = 0
			}
			limits[def.Model] = def.MaxConcurrentSync - r
			if r > 0 {
				reserved[def.Model] = r
			}
		}
	}
	if len(limits) == 0 {
		return nil
	}
	return &ModelSemaphore{rdb: rdb, limits: limits, reserved: reserved}
}

// NewPoolSemaphore builds a ModelSemaphore keyed by backend_pools name instead
// of model, tracking each pool's max_concurrent / priority_reserved_concurrent.
// Uses a distinct Redis key namespace ("gateway:semaphore:pool:") from
// NewModelSemaphore so a pool name colliding with a model name never
// cross-contaminates the two budgets. Returns nil when no pool configures a
// concurrency limit.
func NewPoolSemaphore(pools map[string]config.BackendPoolConfig, rdb *redis.Client) *ModelSemaphore {
	limits := make(map[string]int)
	reserved := make(map[string]int)
	for name, pool := range pools {
		if pool.MaxConcurrent > 0 {
			r := pool.PriorityReservedConcurrent
			if r < 0 || r > pool.MaxConcurrent {
				r = 0
			}
			limits[name] = pool.MaxConcurrent - r
			if r > 0 {
				reserved[name] = r
			}
		}
	}
	if len(limits) == 0 {
		return nil
	}
	return &ModelSemaphore{rdb: rdb, limits: limits, reserved: reserved, keyPrefix: "pool"}
}

// TryAcquire attempts to acquire a sync slot for the given model. isPriority
// requests are tried against the reserved pool first (when one is configured),
// then fall back to the shared pool like any other request.
// Returns ok=false when all applicable slots are busy → caller should return 503.
// usedReserved indicates which pool was acquired from, needed by Release.
// On Redis error, fails open (returns ok=true) to avoid cascading failures.
func (s *ModelSemaphore) TryAcquire(model string, isPriority bool) (ok bool, usedReserved bool) {
	max, tracked := s.limits[model]
	if !tracked {
		return true, false
	}
	reservedMax := s.reserved[model]
	ttlSecs := int(semaphoreTTL.Seconds())

	priorityArg := 0
	if isPriority {
		priorityArg = 1
	}
	res, err := acquireScript.Run(context.Background(), s.rdb,
		[]string{s.reservedKeyFor(model), s.sharedKeyFor(model)},
		reservedMax, max, ttlSecs, priorityArg,
	).Int64Slice()
	if err != nil {
		return true, false // fail open
	}
	return res[0] == 1, res[1] == 1
}

// Release returns a sync slot for the given model after a request completes.
// usedReserved must match what TryAcquire returned for this request.
// No-op when model has no limit configured.
func (s *ModelSemaphore) Release(model string, usedReserved bool) {
	if _, tracked := s.limits[model]; !tracked {
		return
	}
	key := s.sharedKeyFor(model)
	if usedReserved {
		key = s.reservedKeyFor(model)
	}
	_ = releaseScript.Run(context.Background(), s.rdb, []string{key}).Err()
}

// namespace returns the key segment identifying which semaphore family a key
// belongs to: "sync" (default, models) or "pool" (NewPoolSemaphore).
func (s *ModelSemaphore) namespace() string {
	if s.keyPrefix == "" {
		return "sync"
	}
	return s.keyPrefix
}

func (s *ModelSemaphore) sharedKeyFor(key string) string {
	return "gateway:semaphore:" + s.namespace() + ":" + key
}

func (s *ModelSemaphore) reservedKeyFor(key string) string {
	return "gateway:semaphore:" + s.namespace() + ":" + key + ":priority"
}
