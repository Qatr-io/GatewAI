package handler_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"gatewai/gateway/internal/concurrency"
	"gatewai/gateway/internal/config"
	"gatewai/gateway/internal/handler"
	"gatewai/gateway/internal/service"
)

// TestSyncHandler_PriorityReservedPool_SucceedsWhileSharedPoolIsFull verifies
// that once the shared sync concurrency pool is saturated, a request carrying
// server.priority_header still gets through via the reserved pool, while a
// normal request is rejected with 503.
func TestSyncHandler_PriorityReservedPool_SucceedsWhileSharedPoolIsFull(t *testing.T) {
	// reached signals that the blocking request has actually landed in the
	// upstream handler (so the semaphore slot is genuinely held before the
	// test proceeds). release lets the test end the blocking request.
	reached := make(chan struct{}, 1)
	release := make(chan struct{})
	var first int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only the first request to land here blocks (the one meant to
		// saturate the shared pool); later requests respond immediately.
		if atomic.CompareAndSwapInt32(&first, 0, 1) {
			reached <- struct{}{}
			<-release
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer upstream.Close()

	cfgs := []config.ServiceConfig{{
		Type:  "llm",
		Model: "gpt-oss-120b",
		Operations: map[string][]string{
			"chat": {"/v1/chat/completions"},
		},
		InferenceURL:         upstream.URL,
		MaxConcurrentSync:    2, // shared pool = 1, reserved pool = 1
		PriorityReservedSync: 1,
	}}
	reg := service.NewRegistry(cfgs)

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	h := handler.NewSyncHandler(reg, "", nil, nil).
		WithSemaphore(concurrency.NewModelSemaphore(reg, rdb)).
		WithPriorityHeader("X-Priority")

	newReq := func(priority bool) *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"gpt-oss-120b","messages":[]}`))
		req.Header.Set("Content-Type", "application/json")
		if priority {
			req.Header.Set("X-Priority", "1")
		}
		return req
	}

	// Step 1: a normal request occupies the only shared-pool slot, and blocks
	// in-flight until we release it.
	blockingDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, newReq(false))
		blockingDone <- w
	}()
	select {
	case <-reached:
	case <-time.After(2 * time.Second):
		t.Fatal("blocking request never reached the upstream handler")
	}

	// Step 2: a second normal request must be rejected — shared pool is full
	// and normal requests never draw from the reserved pool.
	wNormal := httptest.NewRecorder()
	h.ServeHTTP(wNormal, newReq(false))
	if wNormal.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected normal request to be rejected with 503 once the shared pool is full, got %d: %s",
			wNormal.Code, wNormal.Body.String())
	}

	// Step 3: a priority request must still succeed via the reserved pool,
	// even though the shared pool is completely saturated.
	wPriority := httptest.NewRecorder()
	h.ServeHTTP(wPriority, newReq(true))
	if wPriority.Code != http.StatusOK {
		t.Fatalf("expected priority request to succeed via the reserved pool, got %d: %s",
			wPriority.Code, wPriority.Body.String())
	}

	// Cleanup: release the blocking request.
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
