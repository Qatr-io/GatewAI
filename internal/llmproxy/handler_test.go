package llmproxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"kevent/gateway/internal/cache"
	"kevent/gateway/internal/llmproxy/provider"
	"kevent/gateway/internal/metrics"
	"kevent/gateway/internal/service"
)

// ── in-memory cache ──────────────────────────────────────────────────────────

type memCache struct {
	mu     sync.Mutex
	data   map[string]*cache.Entry
	setted chan struct{} // closed on first Set; nil = no notification needed
}

func newMemCache() *memCache { return &memCache{data: make(map[string]*cache.Entry)} }

// withSetNotify returns a new memCache that closes the returned channel on the
// first successful Set call, allowing tests to wait for async cache-fill.
func newMemCacheWithNotify() (*memCache, <-chan struct{}) {
	ch := make(chan struct{})
	return &memCache{data: make(map[string]*cache.Entry), setted: ch}, ch
}

func (m *memCache) Get(_ context.Context, key string) (*cache.Entry, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.data[key]
	return e, ok, nil
}

func (m *memCache) Set(_ context.Context, key string, entry *cache.Entry, _ time.Duration) error {
	m.mu.Lock()
	ch := m.setted
	m.data[key] = entry
	m.setted = nil // only notify once
	m.mu.Unlock()
	if ch != nil {
		close(ch)
	}
	return nil
}

// ── helpers ──────────────────────────────────────────────────────────────────

func llmDef(provider, backendModel string, cacheTTL time.Duration) *service.Def {
	return &service.Def{
		Type:             "llm",
		Model:            "my-alias",
		Provider:         provider,
		BackendModel:     backendModel,
		ResponseCacheTTL: cacheTTL,
		InferenceURL:     "", // set per-test via setBackend
	}
}

// setBackend points def at a single httptest backend URL.
func setBackend(def *service.Def, url string) {
	def.InferenceURL = url
	def.Backends = []service.Backend{{URL: url, Weight: 1}}
}

func doServeJSON(h *Handler, def *service.Def, body string, extraHeaders ...func(*http.Request)) *httptest.ResponseRecorder {
	return doServeJSONAs(h, def, body, "", extraHeaders...)
}

func doServeJSONAs(h *Handler, def *service.Def, body, consumer string, extraHeaders ...func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for _, fn := range extraHeaders {
		fn(req)
	}
	rr := httptest.NewRecorder()
	h.ServeJSON(rr, req, def, []byte(body), consumer)
	return rr
}

const chatBody = `{"model":"my-alias","messages":[{"role":"user","content":"Hello"}]}`

// openAI-format success response used by fake backends.
const fakeResponse = `{"id":"chatcmpl-1","object":"chat.completion","model":"my-alias","choices":[{"index":0,"message":{"role":"assistant","content":"Hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3}}`

// ── tests ────────────────────────────────────────────────────────────────────

func TestServeJSON_CacheMiss_ThenHit(t *testing.T) {
	// Fake backend returns a valid response.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, fakeResponse)
	}))
	defer backend.Close()

	mc, filled := newMemCacheWithNotify()
	reg := provider.NewRegistry()
	h := New(mc, reg, &http.Client{Timeout: 5 * time.Second}, "", metrics.NoopTracker{}, nil)

	def := llmDef("passthrough", "", 60*time.Second)
	setBackend(def, backend.URL)

	// First call: cache miss.
	rr := doServeJSON(h, def, chatBody)
	if rr.Code != http.StatusOK {
		t.Fatalf("first call: expected 200, got %d", rr.Code)
	}
	if rr.Header().Get("X-Cache") != "MISS" {
		t.Errorf("first call: expected X-Cache=MISS, got %q", rr.Header().Get("X-Cache"))
	}

	// Wait for the async cache-fill goroutine to write the entry.
	select {
	case <-filled:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for async cache-fill")
	}

	// Second call: cache hit.
	rr2 := doServeJSON(h, def, chatBody)
	if rr2.Code != http.StatusOK {
		t.Fatalf("second call: expected 200, got %d", rr2.Code)
	}
	if rr2.Header().Get("X-Cache") != "HIT" {
		t.Errorf("second call: expected X-Cache=HIT, got %q", rr2.Header().Get("X-Cache"))
	}
	if rr2.Body.String() != rr.Body.String() {
		t.Error("cached body should match original response")
	}
}

func TestServeJSON_NoCacheHeader_BypassesCache(t *testing.T) {
	callCount := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, fakeResponse)
	}))
	defer backend.Close()

	mc := newMemCache()
	reg := provider.NewRegistry()
	h := New(mc, reg, &http.Client{Timeout: 5 * time.Second}, "", metrics.NoopTracker{}, nil)

	def := llmDef("passthrough", "", 60*time.Second)
	setBackend(def, backend.URL)

	setNoCache := func(r *http.Request) { r.Header.Set("Cache-Control", "no-cache") }

	doServeJSON(h, def, chatBody, setNoCache)
	doServeJSON(h, def, chatBody, setNoCache)

	if callCount != 2 {
		t.Errorf("Cache-Control: no-cache should bypass cache; backend called %d times (want 2)", callCount)
	}
}

func TestServeJSON_Non200NotCached(t *testing.T) {
	callCount := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":"rate limited"}`)
	}))
	defer backend.Close()

	mc := newMemCache()
	reg := provider.NewRegistry()
	h := New(mc, reg, &http.Client{Timeout: 5 * time.Second}, "", metrics.NoopTracker{}, nil)

	def := llmDef("passthrough", "", 60*time.Second)
	setBackend(def, backend.URL)

	doServeJSON(h, def, chatBody)
	doServeJSON(h, def, chatBody)

	if callCount != 2 {
		t.Errorf("429 responses should not be cached; backend called %d times (want 2)", callCount)
	}
}

func TestServeJSON_CacheDisabled_WhenTTLZero(t *testing.T) {
	callCount := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, fakeResponse)
	}))
	defer backend.Close()

	mc := newMemCache()
	reg := provider.NewRegistry()
	h := New(mc, reg, &http.Client{Timeout: 5 * time.Second}, "", metrics.NoopTracker{}, nil)

	def := llmDef("passthrough", "", 0) // TTL=0 → no cache
	setBackend(def, backend.URL)

	doServeJSON(h, def, chatBody)
	doServeJSON(h, def, chatBody)

	if callCount != 2 {
		t.Errorf("TTL=0 should disable cache; backend called %d times (want 2)", callCount)
	}
}

func TestServeJSON_BackendModel_RewrittenInRequest(t *testing.T) {
	var receivedModel string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		json.Unmarshal(body, &req)
		receivedModel, _ = req["model"].(string)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, fakeResponse)
	}))
	defer backend.Close()

	reg := provider.NewRegistry()
	h := New(cache.NewNoop(), reg, &http.Client{Timeout: 5 * time.Second}, "", metrics.NoopTracker{}, nil)

	def := llmDef("passthrough", "meta-llama/Meta-Llama-3-8B-Instruct", 0)
	setBackend(def, backend.URL)

	// Client sends alias "my-alias".
	doServeJSON(h, def, chatBody)

	if receivedModel != "meta-llama/Meta-Llama-3-8B-Instruct" {
		t.Errorf("backend should receive backend_model; got %q", receivedModel)
	}
}

func TestServeJSON_BackendModel_NotRewritten_WhenEmpty(t *testing.T) {
	var receivedModel string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		json.Unmarshal(body, &req)
		receivedModel, _ = req["model"].(string)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, fakeResponse)
	}))
	defer backend.Close()

	reg := provider.NewRegistry()
	h := New(cache.NewNoop(), reg, &http.Client{Timeout: 5 * time.Second}, "", metrics.NoopTracker{}, nil)

	def := llmDef("passthrough", "", 0) // no backend_model
	setBackend(def, backend.URL)

	doServeJSON(h, def, chatBody)

	// When BackendModel is empty, the alias from the body is forwarded as-is.
	if receivedModel != "my-alias" {
		t.Errorf("without backend_model, original model should be forwarded; got %q", receivedModel)
	}
}

func TestServeJSON_CacheHit_ReturnsCachedBody(t *testing.T) {
	mc := newMemCache()

	// Pre-populate cache with a known entry.
	cacheKey, cacheable, err := cache.Key("passthrough", "my-alias", []byte(chatBody))
	if err != nil || !cacheable {
		t.Fatalf("test setup: cache.Key failed: err=%v cacheable=%v", err, cacheable)
	}
	mc.Set(context.Background(), cacheKey, &cache.Entry{
		Body:        []byte(`{"cached":true}`),
		ContentType: "application/json",
		StatusCode:  200,
	}, 60*time.Second)

	reg := provider.NewRegistry()
	h := New(mc, reg, &http.Client{Timeout: 5 * time.Second}, "", metrics.NoopTracker{}, nil)

	def := llmDef("passthrough", "", 60*time.Second)

	rr := doServeJSON(h, def, chatBody)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if rr.Header().Get("X-Cache") != "HIT" {
		t.Errorf("expected X-Cache=HIT, got %q", rr.Header().Get("X-Cache"))
	}
	if rr.Body.String() != `{"cached":true}` {
		t.Errorf("expected cached body, got %q", rr.Body.String())
	}
}

func TestServeJSON_UnknownProvider_Returns500(t *testing.T) {
	reg := provider.NewRegistry()
	h := New(cache.NewNoop(), reg, &http.Client{Timeout: 5 * time.Second}, "", metrics.NoopTracker{}, nil)

	def := &service.Def{
		Type:     "llm",
		Model:    "x",
		Provider: "nonexistent-provider",
	}

	rr := doServeJSON(h, def, chatBody)
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 for unknown provider, got %d", rr.Code)
	}
}

func TestRewriteBodyModel(t *testing.T) {
	body := []byte(`{"model":"alias","messages":[{"role":"user","content":"hi"}],"temperature":0.5}`)
	out, err := rewriteBodyModel(body, "real-model-id")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	json.Unmarshal(out, &result)

	if result["model"] != "real-model-id" {
		t.Errorf("expected model=real-model-id, got %q", result["model"])
	}
	// Other fields must be preserved.
	if result["temperature"] != 0.5 {
		t.Errorf("expected temperature=0.5, got %v", result["temperature"])
	}
}

func TestRewriteBodyModel_InvalidJSON(t *testing.T) {
	_, err := rewriteBodyModel([]byte(`not json`), "model")
	if err == nil {
		t.Error("expected error for invalid JSON body")
	}
}

// ── consumer tracker ─────────────────────────────────────────────────────────

// testTracker records Track calls for assertion in tests.
type testTracker struct {
	mu   sync.Mutex
	calls []trackCall
}

type trackCall struct {
	consumer, userType, tokenType string
	count                         int
}

func (t *testTracker) Track(_ context.Context, consumer, userType, tokenType string, count int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls = append(t.calls, trackCall{consumer, userType, tokenType, count})
}

func (t *testTracker) sum(consumer, tokenType string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	total := 0
	for _, c := range t.calls {
		if c.consumer == consumer && c.tokenType == tokenType {
			total += c.count
		}
	}
	return total
}

// ── consumer metrics ─────────────────────────────────────────────────────────

func TestServeJSON_ConsumerMetrics_EmittedOnBackendResponse(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"Hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`)
	}))
	defer backend.Close()

	tracker := &testTracker{}
	reg := provider.NewRegistry()
	h := New(newMemCache(), reg, &http.Client{Timeout: 5 * time.Second}, "", tracker, nil)

	def := llmDef("passthrough", "", 0)
	setBackend(def, backend.URL)

	doServeJSONAs(h, def, chatBody, "alice")

	if got := tracker.sum("alice", "prompt"); got != 7 {
		t.Errorf("expected 7 prompt tokens tracked for alice, got %d", got)
	}
}

func TestServeJSON_ConsumerMetrics_EmittedOnCacheHit(t *testing.T) {
	mc := newMemCache()
	cacheKey, _, _ := cache.Key("passthrough", "my-alias", []byte(chatBody))
	mc.Set(context.Background(), cacheKey, &cache.Entry{
		Body:        []byte(`{"id":"c2","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"Hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":2}}`),
		ContentType: "application/json",
		StatusCode:  200,
	}, 60*time.Second)

	tracker := &testTracker{}
	reg := provider.NewRegistry()
	h := New(mc, reg, &http.Client{Timeout: 5 * time.Second}, "", tracker, nil)

	def := llmDef("passthrough", "", 60*time.Second)

	doServeJSONAs(h, def, chatBody, "bob")

	if got := tracker.sum("bob", "completion"); got != 2 {
		t.Errorf("expected 2 completion tokens tracked for bob on cache hit, got %d", got)
	}
}

// ── streaming ────────────────────────────────────────────────────────────────

const streamBody = `{"model":"my-alias","messages":[{"role":"user","content":"Hello"}],"stream":true}`

func TestServeJSON_Streaming_PipedToClient(t *testing.T) {
	sseChunks := "data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\ndata: [DONE]\n\n"
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, sseChunks)
	}))
	defer backend.Close()

	reg := provider.NewRegistry()
	h := New(cache.NewNoop(), reg, &http.Client{Timeout: 5 * time.Second}, "", &noopTracker{}, nil)

	def := llmDef("passthrough", "", 60*time.Second)
	setBackend(def, backend.URL)

	rr := doServeJSON(h, def, streamBody)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if rr.Header().Get("X-Accel-Buffering") != "no" {
		t.Errorf("expected X-Accel-Buffering=no, got %q", rr.Header().Get("X-Accel-Buffering"))
	}
	if rr.Header().Get("Cache-Control") != "no-cache" {
		t.Errorf("expected Cache-Control=no-cache, got %q", rr.Header().Get("Cache-Control"))
	}
	if rr.Body.String() != sseChunks {
		t.Errorf("body mismatch: got %q", rr.Body.String())
	}
}

func TestServeJSON_Streaming_SkipsCache(t *testing.T) {
	callCount := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer backend.Close()

	mc := newMemCache()
	reg := provider.NewRegistry()
	h := New(mc, reg, &http.Client{Timeout: 5 * time.Second}, "", &noopTracker{}, nil)

	def := llmDef("passthrough", "", 60*time.Second)
	setBackend(def, backend.URL)

	doServeJSON(h, def, streamBody)
	doServeJSON(h, def, streamBody)

	if callCount != 2 {
		t.Errorf("streaming should bypass cache: backend called %d times (want 2)", callCount)
	}
	// Cache should not have been written.
	if len(mc.data) != 0 {
		t.Errorf("streaming should not populate cache, got %d entries", len(mc.data))
	}
}

func TestServeJSON_Streaming_BackendModel_Rewritten(t *testing.T) {
	var receivedModel string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &req)
		receivedModel, _ = req["model"].(string)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer backend.Close()

	reg := provider.NewRegistry()
	h := New(cache.NewNoop(), reg, &http.Client{Timeout: 5 * time.Second}, "", &noopTracker{}, nil)

	def := llmDef("passthrough", "real-model-id", 0)
	setBackend(def, backend.URL)

	doServeJSON(h, def, streamBody)

	if receivedModel != "real-model-id" {
		t.Errorf("expected backend to receive real-model-id, got %q", receivedModel)
	}
}

type noopTracker struct{}

func (n *noopTracker) Track(_ context.Context, _, _, _ string, _ int) {}

func TestServeJSON_ConsumerMetrics_SkippedWhenNoConsumer(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, fakeResponse)
	}))
	defer backend.Close()

	tracker := &testTracker{}
	reg := provider.NewRegistry()
	h := New(cache.NewNoop(), reg, &http.Client{Timeout: 5 * time.Second}, "", tracker, nil)

	def := llmDef("passthrough", "", 0)
	setBackend(def, backend.URL)

	rr := doServeJSONAs(h, def, chatBody, "")
	if rr.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rr.Code)
	}
	if len(tracker.calls) != 0 {
		t.Errorf("expected no tracker calls for empty consumer, got %d", len(tracker.calls))
	}
}

// ── Semantic cache mock & tests ───────────────────────────────────────────────

// mockSemanticCache is a controllable in-memory semanticCacher for testing.
type mockSemanticCache struct {
	mu       sync.Mutex
	entry    *cache.Entry // returned on Get if hitBody matches
	hitBody  []byte       // body that triggers a hit (nil = never hit)
	sets     [][]byte     // recorded Set() call bodies
	setCh    chan struct{} // closed on first Set; nil = no notify
}

func newMockSemCache(hitBody []byte, entry *cache.Entry) *mockSemanticCache {
	return &mockSemanticCache{hitBody: hitBody, entry: entry}
}

func newMockSemCacheWithNotify(hitBody []byte, entry *cache.Entry) (*mockSemanticCache, <-chan struct{}) {
	ch := make(chan struct{})
	return &mockSemanticCache{hitBody: hitBody, entry: entry, setCh: ch}, ch
}

func (m *mockSemanticCache) Get(_ context.Context, body []byte) (*cache.Entry, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.hitBody != nil && string(body) == string(m.hitBody) {
		return m.entry, true, nil
	}
	return nil, false, nil
}

func (m *mockSemanticCache) Set(_ context.Context, body []byte, _ *cache.Entry, _ time.Duration) error {
	m.mu.Lock()
	ch := m.setCh
	m.sets = append(m.sets, body)
	m.setCh = nil
	m.mu.Unlock()
	if ch != nil {
		close(ch)
	}
	return nil
}

const semCacheResponse = `{"id":"sem-1","object":"chat.completion","model":"my-alias","choices":[{"index":0,"message":{"role":"assistant","content":"cached"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3}}`

func llmDefWithSemCache(provider, backendModel string, semCacheTTL time.Duration) *service.Def {
	d := llmDef(provider, backendModel, 0)
	d.SemanticCacheTTL = semCacheTTL
	return d
}

// TestServeJSON_SemanticCache_Hit_SkipsBackend verifies that a semantic cache hit
// returns the cached response without calling the backend.
func TestServeJSON_SemanticCache_Hit_SkipsBackend(t *testing.T) {
	backendCalled := false
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	semEntry := &cache.Entry{StatusCode: 200, ContentType: "application/json", Body: []byte(semCacheResponse)}
	sem := newMockSemCache([]byte(chatBody), semEntry)

	reg := provider.NewRegistry()
	h := &Handler{
		cache:      cache.NewNoop(),
		semCache:   sem,
		providers:  reg,
		httpClient: &http.Client{Timeout: 5 * time.Second},
		tracker:    metrics.NoopTracker{},
	}

	def := llmDefWithSemCache("openai", "", 60*time.Second)
	setBackend(def, backend.URL)

	rr := doServeJSON(h, def, chatBody)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rr.Code)
	}
	if backendCalled {
		t.Error("backend must not be called on semantic cache hit")
	}
	if rr.Header().Get("X-Cache") != "SEMANTIC-HIT" {
		t.Errorf("expected X-Cache: SEMANTIC-HIT, got %q", rr.Header().Get("X-Cache"))
	}
	if !strings.Contains(rr.Body.String(), "cached") {
		t.Errorf("expected cached response body, got: %s", rr.Body.String())
	}
}

// TestServeJSON_SemanticCache_Miss_ForwardsToBackend verifies that a semantic miss
// forwards the request to the backend and triggers an async cache fill.
func TestServeJSON_SemanticCache_Miss_ForwardsToBackend(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, fakeResponse)
	}))
	defer backend.Close()

	// miss: hitBody does not match chatBody
	sem, setCh := newMockSemCacheWithNotify([]byte("other body"), nil)

	reg := provider.NewRegistry()
	h := &Handler{
		cache:      cache.NewNoop(),
		semCache:   sem,
		providers:  reg,
		httpClient: &http.Client{Timeout: 5 * time.Second},
		tracker:    metrics.NoopTracker{},
	}

	def := llmDefWithSemCache("openai", "", 60*time.Second)
	setBackend(def, backend.URL)

	rr := doServeJSON(h, def, chatBody)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200 from backend, got %d", rr.Code)
	}
	if rr.Header().Get("X-Cache") == "SEMANTIC-HIT" {
		t.Error("expected no semantic hit header on a miss")
	}

	// Wait for async Set to complete.
	select {
	case <-setCh:
	case <-time.After(2 * time.Second):
		t.Fatal("semantic cache Set was not called after backend response")
	}
	sem.mu.Lock()
	defer sem.mu.Unlock()
	if len(sem.sets) == 0 {
		t.Error("expected semantic cache Set to be called")
	}
}

// TestServeJSON_SemanticCache_Disabled_WhenTTLZero verifies that the semantic cache
// is bypassed when SemanticCacheTTL is 0.
func TestServeJSON_SemanticCache_Disabled_WhenTTLZero(t *testing.T) {
	backendCalled := false
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendCalled = true
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, fakeResponse)
	}))
	defer backend.Close()

	// Would hit if semantic cache were consulted.
	sem := newMockSemCache([]byte(chatBody), &cache.Entry{StatusCode: 200, Body: []byte(semCacheResponse)})

	reg := provider.NewRegistry()
	h := &Handler{
		cache:      cache.NewNoop(),
		semCache:   sem,
		providers:  reg,
		httpClient: &http.Client{Timeout: 5 * time.Second},
		tracker:    metrics.NoopTracker{},
	}

	def := llmDefWithSemCache("openai", "", 0) // TTL=0 → disabled
	setBackend(def, backend.URL)

	doServeJSON(h, def, chatBody)

	if !backendCalled {
		t.Error("backend should be called when SemanticCacheTTL=0")
	}
}

// TestServeJSON_SemanticCache_Streaming_Skipped verifies that streaming requests
// skip the semantic cache entirely and go directly to the backend.
func TestServeJSON_SemanticCache_Streaming_Skipped(t *testing.T) {
	backendCalled := false
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendCalled = true
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer backend.Close()

	streamBody := `{"model":"my-alias","messages":[{"role":"user","content":"Hello"}],"stream":true}`
	// Would hit if semantic cache were consulted.
	sem := newMockSemCache([]byte(streamBody), &cache.Entry{StatusCode: 200, Body: []byte(semCacheResponse)})

	reg := provider.NewRegistry()
	h := &Handler{
		cache:      cache.NewNoop(),
		semCache:   sem,
		providers:  reg,
		httpClient: &http.Client{Timeout: 5 * time.Second},
		tracker:    metrics.NoopTracker{},
	}

	def := llmDefWithSemCache("openai", "", 60*time.Second)
	setBackend(def, backend.URL)

	doServeJSON(h, def, streamBody)

	if !backendCalled {
		t.Error("streaming request must reach the backend, not be served from semantic cache")
	}
}

// TestServeJSON_SemanticCache_NilCache_Passthrough verifies that a nil semCache
// field does not cause a panic and falls through to the backend.
func TestServeJSON_SemanticCache_NilCache_Passthrough(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, fakeResponse)
	}))
	defer backend.Close()

	reg := provider.NewRegistry()
	h := New(cache.NewNoop(), reg, &http.Client{Timeout: 5 * time.Second}, "", metrics.NoopTracker{}, nil)

	def := llmDefWithSemCache("openai", "", 60*time.Second)
	setBackend(def, backend.URL)

	rr := doServeJSON(h, def, chatBody)
	if rr.Code != http.StatusOK {
		t.Errorf("expected 200, got %d (nil semCache should not panic)", rr.Code)
	}
}
