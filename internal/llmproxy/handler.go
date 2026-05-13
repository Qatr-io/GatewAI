// Package llmproxy implements the LLM proxy handler: cache lookup, provider
// routing, response translation, and metric emission for JSON LLM requests.
package llmproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"kevent/gateway/internal/cache"
	"kevent/gateway/internal/cache/semantic"
	"kevent/gateway/internal/llmproxy/provider"
	"kevent/gateway/internal/metrics"
	"kevent/gateway/internal/service"
)

// providerLookup is the subset of provider.Registry used by Handler,
// allowing test injection without depending on the concrete type.
type providerLookup interface {
	Get(name string) (provider.Provider, error)
}

// semanticCacher is the subset of *semantic.Cache used by Handler.
// Extracted as an interface so tests can inject fakes without Redis Stack.
type semanticCacher interface {
	Get(ctx context.Context, body []byte) (*cache.Entry, bool, error)
	Set(ctx context.Context, body []byte, entry *cache.Entry, ttl time.Duration) error
}

// Handler orchestrates LLM requests: cache → provider → translate → cache-fill.
type Handler struct {
	cache          cache.Cache
	semCache       semanticCacher // nil = semantic caching disabled
	providers      providerLookup
	httpClient     *http.Client
	userTypeHeader string // HTTP header carrying consumer type (e.g. "X-User-Type")
	tracker        metrics.ConsumerTracker
}

// New creates a Handler. httpClient should have a generous timeout (e.g. 15 min).
// userTypeHeader is the request header name for the consumer type (e.g. "X-User-Type");
// empty disables user_type labelling. tracker records per-consumer token usage.
// semCache may be nil to disable semantic caching.
func New(c cache.Cache, p *provider.Registry, hc *http.Client, userTypeHeader string, tracker metrics.ConsumerTracker, semCache *semantic.Cache) *Handler {
	var sc semanticCacher
	if semCache != nil {
		sc = semCache
	}
	return &Handler{
		cache:          c,
		semCache:       sc,
		providers:      p,
		httpClient:     hc,
		userTypeHeader: userTypeHeader,
		tracker:        tracker,
	}
}

// ServeJSON handles a JSON-body LLM request. It writes the response to w directly.
// consumer is the authenticated consumer name (e.g. from X-Consumer-Username); empty
// means unauthenticated and per-consumer tracking is skipped.
func (h *Handler) ServeJSON(w http.ResponseWriter, r *http.Request, def *service.Def, body []byte, consumer string) {
	userType := ""
	if h.userTypeHeader != "" {
		userType = r.Header.Get(h.userTypeHeader)
	}
	start := time.Now()
	prov, err := h.providers.Get(def.Provider)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "unknown provider: "+def.Provider)
		return
	}

	// Streaming requests bypass cache and response translation entirely.
	// The SSE stream is piped directly to the client with per-chunk flushing.
	if isStreamingRequest(body) {
		h.serveStream(w, r, def, body, prov, userType, start)
		return
	}

	// Honour Cache-Control: no-cache from client (may appear alongside other
	// directives, e.g. "no-cache, no-store").
	noCache := strings.Contains(r.Header.Get("Cache-Control"), "no-cache")

	// ── Cache lookup ──────────────────────────────────────────────────────────
	var cacheKey string
	var cacheable bool
	if !noCache && def.ResponseCacheTTL > 0 {
		var keyErr error
		cacheKey, cacheable, keyErr = cache.Key(def.Provider, def.Model, body)
		if keyErr != nil {
			slog.WarnContext(r.Context(), "llm cache key error", "error", keyErr)
			metrics.CacheErrorsTotal.WithLabelValues(def.Type, def.Model, "key").Inc()
		}
	}

	if cacheable && cacheKey != "" {
		entry, hit, err := h.cache.Get(r.Context(), cacheKey)
		if err != nil {
			slog.WarnContext(r.Context(), "llm cache get error", "error", err)
			metrics.CacheErrorsTotal.WithLabelValues(def.Type, def.Model, "get").Inc()
		} else if hit {
			metrics.CacheHitsTotal.WithLabelValues(def.Type, def.Model).Inc()
			// Tokens are counted on every delivery (including cache hits) for billing purposes.
			usage := emitTokenMetrics(def, "", userType, entry.Body)
			metrics.LLMRequestsTotal.WithLabelValues(def.Type, def.Model, "", "cache", userType, "200").Inc()
			metrics.LLMRequestDuration.WithLabelValues(def.Type, def.Model, "", "cache", userType).Observe(time.Since(start).Seconds())
			if consumer != "" && usage != nil {
				tCtx := context.WithoutCancel(r.Context())
				h.tracker.Track(tCtx, consumer, userType, "prompt", usage.PromptTokens)
				h.tracker.Track(tCtx, consumer, userType, "completion", usage.CompletionTokens)
			}
			w.Header().Set("Content-Type", entry.ContentType)
			w.Header().Set("X-Cache", "HIT")
			w.WriteHeader(entry.StatusCode)
			_, _ = w.Write(entry.Body)
			return
		}
		metrics.CacheMissesTotal.WithLabelValues(def.Type, def.Model).Inc()
	}

	// ── Semantic cache lookup (after exact miss, before backend) ─────────────
	if h.semCache != nil && def.SemanticCacheTTL > 0 && !isStreamingRequest(body) {
		if semEntry, semHit, semErr := h.semCache.Get(r.Context(), body); semErr != nil {
			slog.WarnContext(r.Context(), "semantic cache get error", "error", semErr)
		} else if semHit {
			metrics.CacheHitsTotal.WithLabelValues(def.Type, def.Model).Inc()
			usage := emitTokenMetrics(def, "", userType, semEntry.Body)
			metrics.LLMRequestsTotal.WithLabelValues(def.Type, def.Model, "", "semantic-cache", userType, "200").Inc()
			metrics.LLMRequestDuration.WithLabelValues(def.Type, def.Model, "", "semantic-cache", userType).Observe(time.Since(start).Seconds())
			if consumer != "" && usage != nil {
				tCtx := context.WithoutCancel(r.Context())
				h.tracker.Track(tCtx, consumer, userType, "prompt", usage.PromptTokens)
				h.tracker.Track(tCtx, consumer, userType, "completion", usage.CompletionTokens)
			}
			ct := semEntry.ContentType
			if ct == "" {
				ct = "application/json"
			}
			w.Header().Set("Content-Type", ct)
			w.Header().Set("X-Cache", "SEMANTIC-HIT")
			w.WriteHeader(semEntry.StatusCode)
			_, _ = w.Write(semEntry.Body)
			return
		}
	}

	// ── Forward to provider (with backend retry) ─────────────────────────────
	// Model rewrite happens per-backend: backend.Model overrides def.BackendModel.
	// Cache key is derived from the alias (def.Model) above, before any rewrite,
	// so cache hits are keyed on the alias regardless of backend model name.
	backends := service.OrderedBackends(def.Backends)
	var resp *http.Response
	var respBody []byte
	var lastBackendErr string
	var winningBackendModel string
	for i, backend := range backends {
		effectiveModel := backend.Model
		if effectiveModel == "" {
			effectiveModel = def.BackendModel
		}
		upstreamBody := body
		if effectiveModel != "" {
			var rewriteErr error
			upstreamBody, rewriteErr = rewriteBodyModel(body, effectiveModel)
			if rewriteErr != nil {
				writeError(w, http.StatusInternalServerError, "failed to rewrite model field: "+rewriteErr.Error())
				return
			}
		}
		upstreamReq, reqErr := prov.BuildRequest(r.Context(), def, upstreamBody, r.URL.Path, backend.URL)
		if reqErr != nil {
			writeError(w, http.StatusInternalServerError, "failed to build upstream request: "+reqErr.Error())
			return
		}
		for k, v := range backend.Headers {
			upstreamReq.Header.Set(k, v)
		}
		var doErr error
		resp, doErr = h.httpClient.Do(upstreamReq)
		if doErr != nil {
			slog.WarnContext(r.Context(), "llm backend error, trying next",
				"backend_index", i, "url", backend.URL, "error", doErr)
			metrics.LLMRequestsTotal.WithLabelValues(def.Type, def.Model, effectiveModel, def.Provider, userType, "502").Inc()
			lastBackendErr = doErr.Error()
			resp = nil
			continue
		}
		if resp.StatusCode >= 500 {
			slog.WarnContext(r.Context(), "llm backend returned 5xx, trying next",
				"backend_index", i, "url", backend.URL, "status", resp.StatusCode)
			metrics.LLMRequestsTotal.WithLabelValues(def.Type, def.Model, effectiveModel, def.Provider, userType,
				strconv.Itoa(resp.StatusCode)).Inc()
			lastBackendErr = fmt.Sprintf("backend returned %d", resp.StatusCode)
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			resp = nil
			continue
		}
		winningBackendModel = effectiveModel
		break // success or 4xx — do not retry
	}
	if resp == nil {
		metrics.LLMRequestsTotal.WithLabelValues(def.Type, def.Model, "", def.Provider, userType, "502").Inc()
		writeError(w, http.StatusBadGateway, "all backends failed: "+lastBackendErr)
		return
	}
	defer resp.Body.Close()

	var readErr error
	respBody, readErr = io.ReadAll(resp.Body)
	if readErr != nil {
		metrics.LLMRequestsTotal.WithLabelValues(def.Type, def.Model, winningBackendModel, def.Provider, userType, "502").Inc()
		writeError(w, http.StatusBadGateway, "failed to read upstream response")
		return
	}

	// ── Translate response ────────────────────────────────────────────────────
	finalStatus, finalBody, usage, err := prov.TranslateResponse(r.Context(), resp.StatusCode, resp.Header, respBody)
	if err != nil {
		slog.ErrorContext(r.Context(), "llm response translation failed", "provider", def.Provider, "error", err)
		metrics.LLMRequestsTotal.WithLabelValues(def.Type, def.Model, winningBackendModel, def.Provider, userType, "500").Inc()
		writeError(w, http.StatusInternalServerError, "failed to translate provider response")
		return
	}

	statusStr := strconv.Itoa(finalStatus)
	metrics.LLMRequestsTotal.WithLabelValues(def.Type, def.Model, winningBackendModel, def.Provider, userType, statusStr).Inc()
	metrics.LLMRequestDuration.WithLabelValues(def.Type, def.Model, winningBackendModel, def.Provider, userType).Observe(time.Since(start).Seconds())

	if usage != nil {
		total := usage.PromptTokens + usage.CompletionTokens
		metrics.LLMTokensTotal.WithLabelValues(def.Type, def.Model, winningBackendModel, userType, "prompt").Add(float64(usage.PromptTokens))
		metrics.LLMTokensTotal.WithLabelValues(def.Type, def.Model, winningBackendModel, userType, "completion").Add(float64(usage.CompletionTokens))
		if total > 0 {
			metrics.LLMTokensPerRequest.WithLabelValues(def.Type, def.Model, winningBackendModel, userType).Observe(float64(total))
		}
		if consumer != "" {
			tCtx := context.WithoutCancel(r.Context())
			h.tracker.Track(tCtx, consumer, userType, "prompt", usage.PromptTokens)
			h.tracker.Track(tCtx, consumer, userType, "completion", usage.CompletionTokens)
		}
	}

	// ── Write response (before cache-fill to avoid blocking the client) ───────
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/json"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("X-Cache", "MISS")
	w.WriteHeader(finalStatus)
	_, _ = w.Write(finalBody)

	// ── Cache-fill async (only 200 responses, non-streaming) ─────────────────
	if finalStatus == http.StatusOK {
		fillEntry := &cache.Entry{
			Body:        finalBody,
			ContentType: "application/json",
			StatusCode:  finalStatus,
		}
		if cacheable && cacheKey != "" {
			ttl := def.ResponseCacheTTL
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := h.cache.Set(ctx, cacheKey, fillEntry, ttl); err != nil {
					slog.Warn("llm cache set error", "error", err)
					metrics.CacheErrorsTotal.WithLabelValues(def.Type, def.Model, "set").Inc()
				}
			}()
		}
		if h.semCache != nil && def.SemanticCacheTTL > 0 {
			reqBody := body
			semTTL := def.SemanticCacheTTL
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				if err := h.semCache.Set(ctx, reqBody, fillEntry, semTTL); err != nil {
					slog.Warn("semantic cache set error", "error", err)
				}
			}()
		}
	}
}

func emitTokenMetrics(def *service.Def, backendModel, userType string, body []byte) *provider.Usage {
	var resp struct {
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil
	}
	if resp.Usage.PromptTokens > 0 {
		metrics.LLMTokensTotal.WithLabelValues(def.Type, def.Model, backendModel, userType, "prompt").Add(float64(resp.Usage.PromptTokens))
	}
	if resp.Usage.CompletionTokens > 0 {
		metrics.LLMTokensTotal.WithLabelValues(def.Type, def.Model, backendModel, userType, "completion").Add(float64(resp.Usage.CompletionTokens))
	}
	total := resp.Usage.PromptTokens + resp.Usage.CompletionTokens
	if total > 0 {
		metrics.LLMTokensPerRequest.WithLabelValues(def.Type, def.Model, backendModel, userType).Observe(float64(total))
	}
	return &provider.Usage{
		PromptTokens:     resp.Usage.PromptTokens,
		CompletionTokens: resp.Usage.CompletionTokens,
	}
}

// rewriteBodyModel replaces the "model" field in a JSON body with newModel.
// The rest of the body is preserved verbatim.
func rewriteBodyModel(body []byte, newModel string) ([]byte, error) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("rewrite model: unmarshal: %w", err)
	}
	raw["model"] = newModel
	out, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("rewrite model: marshal: %w", err)
	}
	return out, nil
}

// isStreamingRequest reports whether the JSON body sets "stream": true.
func isStreamingRequest(body []byte) bool {
	var req struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &req)
	return req.Stream
}

// serveStream pipes a streaming (SSE) LLM response directly to the client.
// Cache and response translation are skipped; chunks are flushed as received.
// Token metrics are not emitted — usage counts are embedded in SSE chunks and
// not parsed to avoid buffering the stream.
// Backend retry is possible before w.WriteHeader; once the SSE stream starts,
// switching backends is no longer possible.
func (h *Handler) serveStream(w http.ResponseWriter, r *http.Request, def *service.Def, body []byte, prov provider.Provider, userType string, start time.Time) {
	backends := service.OrderedBackends(def.Backends)
	var resp *http.Response
	var lastErr string
	var winningBackendModel string
	for i, backend := range backends {
		effectiveModel := backend.Model
		if effectiveModel == "" {
			effectiveModel = def.BackendModel
		}
		upstreamBody := body
		if effectiveModel != "" {
			var rewriteErr error
			upstreamBody, rewriteErr = rewriteBodyModel(body, effectiveModel)
			if rewriteErr != nil {
				writeError(w, http.StatusInternalServerError, "failed to rewrite model field: "+rewriteErr.Error())
				return
			}
		}
		upstreamReq, reqErr := prov.BuildRequest(r.Context(), def, upstreamBody, r.URL.Path, backend.URL)
		if reqErr != nil {
			writeError(w, http.StatusInternalServerError, "failed to build upstream request: "+reqErr.Error())
			return
		}
		for k, v := range backend.Headers {
			upstreamReq.Header.Set(k, v)
		}
		var doErr error
		resp, doErr = h.httpClient.Do(upstreamReq)
		if doErr != nil {
			slog.WarnContext(r.Context(), "llm stream backend error, trying next",
				"backend_index", i, "url", backend.URL, "error", doErr)
			metrics.LLMRequestsTotal.WithLabelValues(def.Type, def.Model, effectiveModel, def.Provider, userType, "502").Inc()
			lastErr = doErr.Error()
			resp = nil
			continue
		}
		if resp.StatusCode >= 500 {
			slog.WarnContext(r.Context(), "llm stream backend returned 5xx, trying next",
				"backend_index", i, "url", backend.URL, "status", resp.StatusCode)
			metrics.LLMRequestsTotal.WithLabelValues(def.Type, def.Model, effectiveModel, def.Provider, userType,
				strconv.Itoa(resp.StatusCode)).Inc()
			lastErr = fmt.Sprintf("backend returned %d", resp.StatusCode)
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			resp = nil
			continue
		}
		winningBackendModel = effectiveModel
		break
	}
	if resp == nil {
		metrics.LLMRequestsTotal.WithLabelValues(def.Type, def.Model, "", def.Provider, userType, "502").Inc()
		writeError(w, http.StatusBadGateway, "all backends failed: "+lastErr)
		return
	}
	defer resp.Body.Close()

	// Forward SSE headers. X-Accel-Buffering: no tells nginx/APISIX to disable
	// proxy buffering so chunks reach the client in real time.
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "text/event-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("X-Cache", "MISS")
	w.WriteHeader(resp.StatusCode)

	statusStr := strconv.Itoa(resp.StatusCode)
	metrics.LLMRequestsTotal.WithLabelValues(def.Type, def.Model, winningBackendModel, def.Provider, userType, statusStr).Inc()
	metrics.LLMRequestDuration.WithLabelValues(def.Type, def.Model, winningBackendModel, def.Provider, userType).Observe(time.Since(start).Seconds())

	// Pipe response, flushing after each read to deliver chunks immediately.
	flusher, canFlush := w.(http.Flusher)
	buf := make([]byte, 4096)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return // client disconnected
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if readErr != nil {
			break
		}
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    fmt.Sprintf("http_%d", status),
		},
	})
	_, _ = w.Write(body)
}
