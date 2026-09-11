package handler_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"gatewai/gateway/internal/cache"
	"gatewai/gateway/internal/concurrency"
	"gatewai/gateway/internal/config"
	"gatewai/gateway/internal/handler"
	"gatewai/gateway/internal/llmproxy"
	"gatewai/gateway/internal/llmproxy/provider"
	"gatewai/gateway/internal/metrics"
	"gatewai/gateway/internal/ratelimit"
	"gatewai/gateway/internal/service"
)

// stubPoolChecker implements ratelimit.PoolChecker for tests: it rejects a
// call when (poolName, memberURL) is in the deny set, allows everything else.
type stubPoolChecker struct {
	deny map[string]bool // key: poolName + "|" + memberURL
	err  error           // when set, every call returns this error (still fail-open per contract)
}

func (s *stubPoolChecker) CheckBackendPool(_ context.Context, poolName, memberURL string) (ratelimit.CheckResult, error) {
	if s.err != nil {
		return ratelimit.CheckResult{Allowed: true}, s.err
	}
	if s.deny[poolName+"|"+memberURL] {
		return ratelimit.CheckResult{Allowed: false}, nil
	}
	return ratelimit.CheckResult{Allowed: true}, nil
}

func newPoolSemaphoreForTest(t *testing.T, poolName string, maxConcurrent, priorityReserved int) *concurrency.ModelSemaphore {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	sem := concurrency.NewPoolSemaphore(map[string]config.BackendPoolConfig{
		poolName: {MaxConcurrent: maxConcurrent, PriorityReservedConcurrent: priorityReserved},
	}, rdb)
	if sem == nil {
		t.Fatal("expected non-nil pool semaphore")
	}
	return sem
}

// poolRegistry builds a registry with one non-LLM JSON service (no provider
// set, so it always takes the direct-proxy path through proxyToInference)
// backed by a backend_pools entry with the given member URLs.
func poolRegistry(poolName string, urls ...string) *service.Registry {
	members := make([]config.BackendPoolMember, len(urls))
	for i, u := range urls {
		members[i] = config.BackendPoolMember{BackendConfig: config.BackendConfig{URL: u, Weight: 1}}
	}
	cfgs := []config.ServiceConfig{{
		Type:  "llm",
		Model: "pooled-model",
		Operations: map[string][]string{
			"chat": {"/v1/chat/completions"},
		},
		BackendPool: poolName,
	}}
	return service.NewRegistry(cfgs, service.WithBackendPools(map[string]config.BackendPoolConfig{
		poolName: {Members: members},
	}))
}

func newJSONReq() *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"pooled-model","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestProxyToInference_PoolSemaphore_Saturated_Returns503_NoBackendCall(t *testing.T) {
	const poolName = "vllm-pool"
	var calls int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	reg := poolRegistry(poolName, upstream.URL)
	poolSem := newPoolSemaphoreForTest(t, poolName, 1, 0)

	// Exhaust the pool's only slot directly.
	ok, _ := poolSem.TryAcquire(poolName, false)
	if !ok {
		t.Fatal("expected to acquire the only pool slot")
	}

	h := handler.NewSyncHandler(reg, "", nil, nil).WithPoolSemaphore(poolSem)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newJSONReq())

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", w.Code, w.Body.String())
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatalf("expected no backend call when pool is saturated, got %d calls", calls)
	}
}

func TestProxyToInference_PoolSemaphore_ReleasedAfterSuccess(t *testing.T) {
	const poolName = "vllm-pool"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	reg := poolRegistry(poolName, upstream.URL)
	poolSem := newPoolSemaphoreForTest(t, poolName, 1, 0)
	h := handler.NewSyncHandler(reg, "", nil, nil).WithPoolSemaphore(poolSem)

	// Two sequential requests must both succeed: the first must release its
	// slot before (or as) it returns, so the second can acquire it.
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, newJSONReq())
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: expected 200, got %d: %s", i, w.Code, w.Body.String())
		}
	}
}

func TestProxyToInference_PoolSemaphore_ReleasedAfterAllBackendsFail(t *testing.T) {
	const poolName = "vllm-pool"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	reg := poolRegistry(poolName, upstream.URL)
	poolSem := newPoolSemaphoreForTest(t, poolName, 1, 0)
	h := handler.NewSyncHandler(reg, "", nil, nil).
		WithPoolSemaphore(poolSem).
		WithRetryBackoff(time.Millisecond)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newJSONReq())
	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 after all backends fail, got %d: %s", w.Code, w.Body.String())
	}

	// The slot must have been released — a follow-up request must be able to acquire it.
	ok, _ := poolSem.TryAcquire(poolName, false)
	if !ok {
		t.Fatal("expected pool slot to be free after the failed request released it")
	}
}

func TestProxyToInference_PoolRateLimit_SkipsMemberThenSucceedsOnNext(t *testing.T) {
	const poolName = "vllm-pool"
	badUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("rate-limited backend must not be called")
	}))
	defer badUp.Close()
	goodUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer goodUp.Close()

	reg := poolRegistry(poolName, badUp.URL, goodUp.URL)
	pc := &stubPoolChecker{deny: map[string]bool{poolName + "|" + badUp.URL: true}}
	h := handler.NewSyncHandler(reg, "", nil, nil).WithPoolLimiter(pc)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newJSONReq())
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 from the non-rate-limited member, got %d: %s", w.Code, w.Body.String())
	}
}

func TestProxyToInference_PoolRateLimit_AllMembersRejected_Returns502(t *testing.T) {
	const poolName = "vllm-pool"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("rate-limited backend must not be called")
	}))
	defer up.Close()

	reg := poolRegistry(poolName, up.URL)
	pc := &stubPoolChecker{deny: map[string]bool{poolName + "|" + up.URL: true}}
	h := handler.NewSyncHandler(reg, "", nil, nil).
		WithPoolLimiter(pc).
		WithRetryBackoff(time.Millisecond)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newJSONReq())
	// proxyToInference's outer retry loop only special-cases circuit-open in
	// llmproxy; here exhausting retries with every member pool-rate-limited
	// falls through to the existing generic 502, not a fast-fail 503.
	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 when all members are pool-rate-limited, got %d: %s", w.Code, w.Body.String())
	}
}

func TestProxyToInference_NonPoolService_PoolLimiterAndSemaphoreSet_NoBehaviorChange(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	cfgs := []config.ServiceConfig{{
		Type:  "llm",
		Model: "plain-model",
		Operations: map[string][]string{
			"chat": {"/v1/chat/completions"},
		},
		InferenceURL: upstream.URL, // no BackendPool set
	}}
	reg := service.NewRegistry(cfgs)

	poolSem := newPoolSemaphoreForTest(t, "some-pool", 1, 0)
	// Saturate it — if the handler mistakenly consulted it for a non-pool
	// service, this would cause a spurious 503.
	if ok, _ := poolSem.TryAcquire("some-pool", false); !ok {
		t.Fatal("expected to acquire the pool slot")
	}
	pc := &stubPoolChecker{deny: map[string]bool{"some-pool|" + upstream.URL: true}}

	h := handler.NewSyncHandler(reg, "", nil, nil).
		WithPoolSemaphore(poolSem).
		WithPoolLimiter(pc)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"plain-model","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected non-pool service to be unaffected by globally-wired pool checkers, got %d: %s", w.Code, w.Body.String())
	}
}

// TestSyncHandler_LLMDelegatedRequest_PoolSemaphoreAcquiredOnceNotTwice is a
// regression/documentation guard for the double-acquisition bug avoided while
// wiring Phase 6: SyncHandler.handleJSON delegates LLM-provider requests to
// llmproxy.Handler.ServeJSON, which independently enforces backend_pools
// concurrency. SyncHandler itself (via proxyToInference) must NOT also
// acquire a slot for that same delegated request, or every LLM request
// routed through a pool would silently consume two slots instead of one.
//
// With a 2-slot pool: one in-flight (blocked) LLM request must leave exactly
// one slot free for a second concurrent LLM request. If SyncHandler ever
// double-acquired, the first request alone would exhaust both slots and the
// second would be spuriously rejected with 503.
func TestSyncHandler_LLMDelegatedRequest_PoolSemaphoreAcquiredOnceNotTwice(t *testing.T) {
	const poolName = "vllm-pool"

	reached := make(chan struct{}, 1)
	release := make(chan struct{})
	var first int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.CompareAndSwapInt32(&first, 0, 1) {
			reached <- struct{}{}
			<-release
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hi"}}]}`))
	}))
	defer upstream.Close()

	members := []config.BackendPoolMember{{BackendConfig: config.BackendConfig{URL: upstream.URL, Weight: 1}}}
	cfgs := []config.ServiceConfig{{
		Type:     "llm",
		Model:    "gpt-4o",
		Provider: "openai",
		Operations: map[string][]string{
			"chat": {"/v1/chat/completions"},
		},
		BackendPool: poolName,
	}}
	reg := service.NewRegistry(cfgs, service.WithBackendPools(map[string]config.BackendPoolConfig{
		poolName: {Members: members},
	}))

	poolSem := newPoolSemaphoreForTest(t, poolName, 2, 0) // 2 shared slots, no reservation

	llmHandler := llmproxy.New(cache.NewNoop(), provider.NewRegistry(), &http.Client{Timeout: 5 * time.Second}, "", metrics.NoopTracker{}, llmproxy.AuditConfig{}, nil).
		WithPoolSemaphore(poolSem)

	// Deliberately wire the same pool semaphore into SyncHandler too (as
	// production main.go does) to prove its own field being set does not
	// cause a second acquisition for requests it delegates to llmHandler.
	h := handler.NewSyncHandler(reg, "", nil, llmHandler).
		WithPoolSemaphore(poolSem)

	newLLMReq := func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Content-Type", "application/json")
		return req
	}

	blockingDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, newLLMReq())
		blockingDone <- w
	}()
	select {
	case <-reached:
	case <-time.After(2 * time.Second):
		t.Fatal("blocking request never reached the upstream handler")
	}

	// A single in-flight LLM request must consume exactly one of the pool's
	// two slots — a second concurrent request must still succeed.
	wSecond := httptest.NewRecorder()
	h.ServeHTTP(wSecond, newLLMReq())
	if wSecond.Code != http.StatusOK {
		t.Fatalf("expected second concurrent LLM request to succeed via the pool's remaining slot (proves single-acquisition), got %d: %s",
			wSecond.Code, wSecond.Body.String())
	}

	close(release)
	select {
	case w := <-blockingDone:
		if w.Code != http.StatusOK {
			t.Fatalf("expected the blocking request to eventually succeed, got %d", w.Code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("blocking request never completed after release")
	}
}

func TestProxyToInference_Multipart_PoolSemaphore_Saturated_Returns503(t *testing.T) {
	const poolName = "vllm-pool"
	var calls int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	members := []config.BackendPoolMember{{BackendConfig: config.BackendConfig{URL: upstream.URL, Weight: 1}}}
	cfgs := []config.ServiceConfig{{
		Type:  "transcription",
		Model: "pooled-model",
		Operations: map[string][]string{
			"transcription": {"/v1/audio/transcriptions"},
		},
		BackendPool: poolName,
	}}
	reg := service.NewRegistry(cfgs, service.WithBackendPools(map[string]config.BackendPoolConfig{
		poolName: {Members: members},
	}))

	poolSem := newPoolSemaphoreForTest(t, poolName, 1, 0)
	if ok, _ := poolSem.TryAcquire(poolName, false); !ok {
		t.Fatal("expected to acquire the only pool slot")
	}

	h := handler.NewSyncHandler(reg, "", nil, nil).WithPoolSemaphore(poolSem)

	req := multipartRequest(t, "/v1/audio/transcriptions", "pooled-model", []byte("fake audio"))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", w.Code, w.Body.String())
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatalf("expected no backend call when pool is saturated, got %d calls", calls)
	}
}
