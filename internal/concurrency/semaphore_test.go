package concurrency_test

import (
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"gatewai/gateway/internal/concurrency"
	"gatewai/gateway/internal/config"
	"gatewai/gateway/internal/service"
)

func newSemaphore(t *testing.T, maxConcurrentSync, priorityReservedSync int) *concurrency.ModelSemaphore {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	reg := service.NewRegistry([]config.ServiceConfig{{
		Type:                 "llm",
		Model:                "gpt-oss-120b",
		MaxConcurrentSync:    maxConcurrentSync,
		PriorityReservedSync: priorityReservedSync,
	}})
	sem := concurrency.NewModelSemaphore(reg, rdb)
	if sem == nil {
		t.Fatal("expected non-nil semaphore")
	}
	return sem
}

func TestModelSemaphore_NoReservation_UnchangedBehavior(t *testing.T) {
	sem := newSemaphore(t, 2, 0)

	ok1, reserved1 := sem.TryAcquire("gpt-oss-120b", false)
	ok2, reserved2 := sem.TryAcquire("gpt-oss-120b", true) // no reservation configured: priority competes in shared pool too
	if !ok1 || reserved1 || !ok2 || reserved2 {
		t.Fatalf("expected both requests to acquire the shared pool, got (%v,%v) (%v,%v)", ok1, reserved1, ok2, reserved2)
	}
	if ok, _ := sem.TryAcquire("gpt-oss-120b", false); ok {
		t.Fatal("expected shared pool to be exhausted")
	}
}

func TestModelSemaphore_ReservedPool_AlwaysAvailableToPriority(t *testing.T) {
	sem := newSemaphore(t, 3, 1) // shared pool = 2, reserved pool = 1

	ok1, r1 := sem.TryAcquire("gpt-oss-120b", false)
	ok2, r2 := sem.TryAcquire("gpt-oss-120b", false)
	if !ok1 || r1 || !ok2 || r2 {
		t.Fatalf("expected 2 non-priority requests to fill the shared pool, got (%v,%v) (%v,%v)", ok1, r1, ok2, r2)
	}

	// Shared pool is now full; a non-priority request must be rejected.
	if ok, _ := sem.TryAcquire("gpt-oss-120b", false); ok {
		t.Fatal("expected non-priority request to be rejected once shared pool is full")
	}

	// A priority request must still succeed via the reserved pool.
	okP, reservedP := sem.TryAcquire("gpt-oss-120b", true)
	if !okP || !reservedP {
		t.Fatalf("expected priority request to acquire the reserved pool, got ok=%v usedReserved=%v", okP, reservedP)
	}

	// Reserved pool (size 1) is now full too; a second priority request must fail.
	if ok, _ := sem.TryAcquire("gpt-oss-120b", true); ok {
		t.Fatal("expected second priority request to be rejected once reserved pool is also full")
	}
}

func TestModelSemaphore_PriorityFallsBackToSharedPool(t *testing.T) {
	sem := newSemaphore(t, 2, 1) // shared pool = 1, reserved pool = 1

	// Take the reserved slot first.
	okR, reservedR := sem.TryAcquire("gpt-oss-120b", true)
	if !okR || !reservedR {
		t.Fatalf("expected first priority request to acquire the reserved pool, got ok=%v usedReserved=%v", okR, reservedR)
	}

	// A second priority request should fall back to (and succeed via) the shared pool.
	okR2, reservedR2 := sem.TryAcquire("gpt-oss-120b", true)
	if !okR2 || reservedR2 {
		t.Fatalf("expected second priority request to fall back to the shared pool, got ok=%v usedReserved=%v", okR2, reservedR2)
	}

	// Shared pool is now full; even a priority request (reserved already full) must be rejected.
	if ok, _ := sem.TryAcquire("gpt-oss-120b", true); ok {
		t.Fatal("expected third priority request to be rejected once both pools are full")
	}
}

func TestModelSemaphore_ReleaseFreesCorrectPool(t *testing.T) {
	sem := newSemaphore(t, 2, 1) // shared pool = 1, reserved pool = 1

	okR, reservedR := sem.TryAcquire("gpt-oss-120b", true)
	if !okR || !reservedR {
		t.Fatalf("expected priority request to acquire reserved pool, got ok=%v usedReserved=%v", okR, reservedR)
	}
	sem.Release("gpt-oss-120b", reservedR)

	// Reserved pool should be free again.
	okR2, reservedR2 := sem.TryAcquire("gpt-oss-120b", true)
	if !okR2 || !reservedR2 {
		t.Fatalf("expected reserved pool to be free after release, got ok=%v usedReserved=%v", okR2, reservedR2)
	}
}

func TestModelSemaphore_ZeroDiffKeyName_NoReservation(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	reg := service.NewRegistry([]config.ServiceConfig{{
		Type:              "llm",
		Model:             "gpt-oss-120b",
		MaxConcurrentSync: 1,
	}})
	sem := concurrency.NewModelSemaphore(reg, rdb)

	ok, _ := sem.TryAcquire("gpt-oss-120b", false)
	if !ok {
		t.Fatal("expected acquire to succeed")
	}

	if !mr.Exists("gateway:semaphore:sync:gpt-oss-120b") {
		t.Fatal("expected the existing shared counter key to be used unchanged")
	}
	keys := mr.Keys()
	for _, k := range keys {
		if k == "gateway:semaphore:sync:gpt-oss-120b:priority" {
			t.Fatal("did not expect a reserved-pool key when no reservation is configured")
		}
	}
}

func TestNewPoolSemaphore_NoLimitsConfigured_ReturnsNil(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	sem := concurrency.NewPoolSemaphore(map[string]config.BackendPoolConfig{
		"vllm-llama3": {MaxConcurrent: 0},
	}, rdb)
	if sem != nil {
		t.Fatal("expected nil semaphore when no pool configures max_concurrent")
	}

	sem = concurrency.NewPoolSemaphore(nil, rdb)
	if sem != nil {
		t.Fatal("expected nil semaphore for empty pools map")
	}
}

func TestNewPoolSemaphore_BuildsLimitsCorrectly(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	sem := concurrency.NewPoolSemaphore(map[string]config.BackendPoolConfig{
		"vllm-llama3": {MaxConcurrent: 3, PriorityReservedConcurrent: 1}, // shared = 2, reserved = 1
	}, rdb)
	if sem == nil {
		t.Fatal("expected non-nil semaphore")
	}

	ok1, r1 := sem.TryAcquire("vllm-llama3", false)
	ok2, r2 := sem.TryAcquire("vllm-llama3", false)
	if !ok1 || r1 || !ok2 || r2 {
		t.Fatalf("expected 2 non-priority requests to fill the shared pool, got (%v,%v) (%v,%v)", ok1, r1, ok2, r2)
	}
	if ok, _ := sem.TryAcquire("vllm-llama3", false); ok {
		t.Fatal("expected shared pool to be exhausted")
	}
	okP, reservedP := sem.TryAcquire("vllm-llama3", true)
	if !okP || !reservedP {
		t.Fatalf("expected priority request to acquire the reserved pool, got ok=%v usedReserved=%v", okP, reservedP)
	}
}

func TestNewPoolSemaphore_NamespaceDistinctFromModelSemaphore(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	// A pool and a model share the exact same name — their Redis keys must never collide.
	const sharedName = "gpt-oss-120b"

	poolSem := concurrency.NewPoolSemaphore(map[string]config.BackendPoolConfig{
		sharedName: {MaxConcurrent: 1},
	}, rdb)
	if poolSem == nil {
		t.Fatal("expected non-nil pool semaphore")
	}

	reg := service.NewRegistry([]config.ServiceConfig{{
		Type:              "llm",
		Model:             sharedName,
		MaxConcurrentSync: 1,
	}})
	modelSem := concurrency.NewModelSemaphore(reg, rdb)
	if modelSem == nil {
		t.Fatal("expected non-nil model semaphore")
	}

	// Exhaust the pool's slot.
	okPool, _ := poolSem.TryAcquire(sharedName, false)
	if !okPool {
		t.Fatal("expected pool semaphore to acquire its slot")
	}

	// The model semaphore, keyed by the same name, must be unaffected.
	okModel, _ := modelSem.TryAcquire(sharedName, false)
	if !okModel {
		t.Fatal("expected model semaphore to acquire independently of the pool semaphore's identically-named slot")
	}

	if !mr.Exists("gateway:semaphore:pool:" + sharedName) {
		t.Fatal("expected pool semaphore to use the gateway:semaphore:pool: namespace")
	}
	if !mr.Exists("gateway:semaphore:sync:" + sharedName) {
		t.Fatal("expected model semaphore to keep using the gateway:semaphore:sync: namespace")
	}
}
