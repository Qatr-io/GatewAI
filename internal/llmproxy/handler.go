// Package llmproxy implements the LLM proxy handler: cache lookup, provider
// routing, response translation, and metric emission for JSON LLM requests.
package llmproxy

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"gatewai/gateway/internal/cache"
	"gatewai/gateway/internal/guardrails"
	"gatewai/gateway/internal/llmproxy/provider"
	"gatewai/gateway/internal/metrics"
	"gatewai/gateway/internal/ratelimit"
	"gatewai/gateway/internal/service"
)

// providerLookup is the subset of provider.Registry used by Handler,
// allowing test injection without depending on the concrete type.
type providerLookup interface {
	Get(name string) (provider.Provider, error)
}

// AuditConfig controls structured per-request audit logging.
// Mirrors config.AuditLogConfig but kept local to avoid importing the config package.
type AuditConfig struct {
	Enabled bool // emit a structured slog record for every LLM request
	Prompt  bool // include the raw request body (opt-in — may contain PII)
}

// Handler orchestrates LLM requests: cache → provider → translate → cache-fill.
type Handler struct {
	cache          cache.Cache
	providers      providerLookup
	httpClient     *http.Client
	userTypeHeader string // HTTP header carrying consumer type (e.g. "X-User-Type")
	tracker        metrics.ConsumerTracker
	audit          AuditConfig
	tokenLimiter   ratelimit.TokenChecker // nil = token rate limiting disabled
	guard          *guardrails.Checker    // output DLP scanner
}

// New creates a Handler. httpClient should have a generous timeout (e.g. 15 min).
// userTypeHeader is the request header name for the consumer type (e.g. "X-User-Type");
// empty disables user_type labelling. tracker records per-consumer token usage.
// audit controls structured audit logging (disabled when audit.Enabled == false).
// tl is the optional token rate limiter; nil disables token budget enforcement.
func New(c cache.Cache, p *provider.Registry, hc *http.Client, userTypeHeader string, tracker metrics.ConsumerTracker, audit AuditConfig, tl ratelimit.TokenChecker) *Handler {
	return &Handler{
		cache:          c,
		providers:      p,
		httpClient:     hc,
		userTypeHeader: userTypeHeader,
		tracker:        tracker,
		audit:          audit,
		tokenLimiter:   tl,
		guard:          guardrails.New(),
	}
}

// checkAndWriteTokenLimit checks the service-level and model-level token budgets.
// Returns true when the request is allowed to proceed. On rejection it writes
// HTTP 429 (with X-TokenRateLimit-* headers) and returns false — callers must
// return immediately.
func (h *Handler) checkAndWriteTokenLimit(w http.ResponseWriter, r *http.Request, serviceType, model string) bool {
	if h.tokenLimiter == nil {
		return true
	}
	res, err := h.tokenLimiter.CheckTokens(r.Context(), r, serviceType)
	if err != nil {
		slog.WarnContext(r.Context(), "token rate limit check error", "error", err)
	}
	if !res.Allowed {
		writeTokenLimitHeaders(w, res)
		writeError(w, http.StatusTooManyRequests, "token rate limit exceeded")
		return false
	}
	if model == "" {
		return true
	}
	mres, merr := h.tokenLimiter.CheckModelTokens(r.Context(), r, model)
	if merr != nil {
		slog.WarnContext(r.Context(), "model token rate limit check error", "error", merr)
	}
	if !mres.Allowed {
		writeTokenLimitHeaders(w, mres)
		writeError(w, http.StatusTooManyRequests, "token rate limit exceeded")
		return false
	}
	return true
}

// ServeJSON handles a JSON-body LLM request. It writes the response to w directly.
// consumer is the authenticated consumer name (e.g. from X-Consumer-Username); empty
// means unauthenticated and per-consumer tracking is skipped.
func (h *Handler) ServeJSON(w http.ResponseWriter, r *http.Request, def *service.Def, body []byte, consumer string) {
	userType := ""
	if h.userTypeHeader != "" {
		userType = r.Header.Get(h.userTypeHeader)
	}

	ctx, span := otel.Tracer("gatewai/gateway").Start(r.Context(), "gateway.llm.request",
		trace.WithAttributes(
			attribute.String("service_type", def.Type),
			attribute.String("model", def.Model),
			attribute.String("provider", def.Provider),
			attribute.String("consumer", consumer),
		))
	defer span.End()
	r = r.WithContext(ctx)

	start := time.Now()
	prov, err := h.providers.Get(def.Provider)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "unknown provider: "+def.Provider)
		return
	}

	// Streaming requests bypass cache and response translation entirely.
	// The SSE stream is piped directly to the client with per-chunk flushing.
	if isStreamingRequest(body) {
		h.serveStream(w, r, def, body, prov, consumer, userType, start)
		return
	}

	if !h.checkAndWriteTokenLimit(w, r, def.Type, def.Model) {
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
			usage := emitTokenMetrics(ctx, def, "", userType, entry.Body)
			metrics.LLMRequestsTotal.WithLabelValues(def.Type, def.Model, "", "cache", userType, "200").Inc()
			metrics.ObserveWithExemplar(ctx, metrics.LLMRequestDuration.WithLabelValues(def.Type, def.Model, "", "cache", userType), time.Since(start).Seconds())
			if consumer != "" && usage != nil {
				tCtx := context.WithoutCancel(r.Context())
				h.tracker.Track(tCtx, consumer, userType, "prompt", usage.PromptTokens)
				h.tracker.Track(tCtx, consumer, userType, "completion", usage.CompletionTokens)
			}
			w.Header().Set("Content-Type", entry.ContentType)
			w.Header().Set("X-Cache", "HIT")
			w.WriteHeader(entry.StatusCode)
			_, _ = w.Write(entry.Body)
			if h.tokenLimiter != nil && usage != nil {
				total := usage.PromptTokens + usage.CompletionTokens
				if total > 0 {
					tCtx := context.WithoutCancel(r.Context())
					_ = h.tokenLimiter.AddTokens(tCtx, r, def.Type, total)
					if def.Model != "" {
						_ = h.tokenLimiter.AddModelTokens(tCtx, r, def.Model, total)
					}
				}
			}
			return
		}
		metrics.CacheMissesTotal.WithLabelValues(def.Type, def.Model).Inc()
	}

	// ── Forward to provider (with backend retry) ─────────────────────────────
	// Model rewrite happens per-backend: backend.Model overrides def.BackendModel.
	// Cache key is derived from the alias (def.Model) above, before any rewrite,
	// so cache hits are keyed on the alias regardless of backend model name.
	backends := service.OrderedBackends(def.Backends)
	var resp *http.Response
	var respBody []byte
	var lastBackendErr string
	var winningBackendModel, winningBackendURL string
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
		otel.GetTextMapPropagator().Inject(r.Context(), propagation.HeaderCarrier(upstreamReq.Header))
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
		winningBackendURL = backend.URL
		break // success or 4xx — do not retry
	}
	if resp == nil {
		span.RecordError(fmt.Errorf("all backends failed: %s", lastBackendErr))
		span.SetStatus(codes.Error, "all backends failed")
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
	span.SetAttributes(
		attribute.Int("http.status_code", finalStatus),
		attribute.String("llm.backend_model", winningBackendModel),
	)
	if finalStatus >= 500 {
		span.SetStatus(codes.Error, "backend error")
	}
	metrics.LLMRequestsTotal.WithLabelValues(def.Type, def.Model, winningBackendModel, def.Provider, userType, statusStr).Inc()
	metrics.ObserveWithExemplar(ctx, metrics.LLMRequestDuration.WithLabelValues(def.Type, def.Model, winningBackendModel, def.Provider, userType), time.Since(start).Seconds())

	if usage != nil {
		total := usage.PromptTokens + usage.CompletionTokens
		metrics.LLMTokensTotal.WithLabelValues(def.Type, def.Model, winningBackendModel, userType, "prompt").Add(float64(usage.PromptTokens))
		metrics.LLMTokensTotal.WithLabelValues(def.Type, def.Model, winningBackendModel, userType, "completion").Add(float64(usage.CompletionTokens))
		if total > 0 {
			metrics.ObserveWithExemplar(ctx, metrics.LLMTokensPerRequest.WithLabelValues(def.Type, def.Model, winningBackendModel, userType), float64(total))
		}
		if consumer != "" {
			tCtx := context.WithoutCancel(r.Context())
			h.tracker.Track(tCtx, consumer, userType, "prompt", usage.PromptTokens)
			h.tracker.Track(tCtx, consumer, userType, "completion", usage.CompletionTokens)
		}
	}

	// ── Output DLP guardrails ─────────────────────────────────────────────────
	// Applied after response translation; only on successful (2xx) responses.
	outputBlocked := false
	if def.Guardrails.Output.Enabled && h.guard != nil && finalStatus < 300 {
		switch def.Guardrails.Output.Action {
		case "redact":
			redacted, found := h.guard.RedactResponse(finalBody, def.Guardrails.Output.Checks)
			if len(found) > 0 {
				finalBody = redacted
				slog.WarnContext(r.Context(), "llm response redacted by output guardrails",
					"service_type", def.Type, "model", def.Model, "consumer", consumer, "violations", found)
				metrics.GuardrailsTotal.WithLabelValues(def.Type, def.Model, "output", "redact", "redacted").Inc()
			}
		case "flag":
			if found := h.guard.ScanResponse(finalBody, def.Guardrails.Output.Checks); len(found) > 0 {
				slog.WarnContext(r.Context(), "llm response flagged by output guardrails",
					"service_type", def.Type, "model", def.Model, "consumer", consumer, "violations", found)
				metrics.GuardrailsTotal.WithLabelValues(def.Type, def.Model, "output", "flag", "flagged").Inc()
			}
		default: // "block"
			if found := h.guard.ScanResponse(finalBody, def.Guardrails.Output.Checks); len(found) > 0 {
				slog.WarnContext(r.Context(), "llm response blocked by output guardrails",
					"service_type", def.Type, "model", def.Model, "consumer", consumer, "violations", found)
				metrics.GuardrailsTotal.WithLabelValues(def.Type, def.Model, "output", "block", "blocked").Inc()
				finalStatus = http.StatusUnprocessableEntity
				finalBody = []byte(`{"error":"response blocked by guardrails"}`)
				outputBlocked = true
			}
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

	if h.tokenLimiter != nil && usage != nil {
		total := usage.PromptTokens + usage.CompletionTokens
		if total > 0 {
			tCtx := context.WithoutCancel(r.Context())
			_ = h.tokenLimiter.AddTokens(tCtx, r, def.Type, total)
			if def.Model != "" {
				_ = h.tokenLimiter.AddModelTokens(tCtx, r, def.Model, total)
			}
		}
	}

	h.auditLog(r.Context(), def, consumer, userType, winningBackendURL, winningBackendModel,
		finalStatus, time.Since(start).Milliseconds(), false, body, usage)

	// ── Cache-fill async (only 200 responses, non-streaming, not output-blocked) ─
	if !outputBlocked && cacheable && cacheKey != "" && finalStatus == http.StatusOK {
		entry := &cache.Entry{
			Body:        finalBody,
			ContentType: "application/json",
			StatusCode:  finalStatus,
		}
		ttl := def.ResponseCacheTTL
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := h.cache.Set(ctx, cacheKey, entry, ttl); err != nil {
				slog.Warn("llm cache set error", "error", err)
				metrics.CacheErrorsTotal.WithLabelValues(def.Type, def.Model, "set").Inc()
			}
		}()
	}
}

func emitTokenMetrics(ctx context.Context, def *service.Def, backendModel, userType string, body []byte) *provider.Usage {
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
		metrics.ObserveWithExemplar(ctx, metrics.LLMTokensPerRequest.WithLabelValues(def.Type, def.Model, backendModel, userType), float64(total))
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

// injectStreamUsage adds stream_options.include_usage=true to a JSON body so
// OpenAI-compatible backends return a usage chunk at the end of the stream.
// The original body is returned unchanged on any parse error.
func injectStreamUsage(body []byte) []byte {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return body
	}
	opts, _ := raw["stream_options"].(map[string]any)
	if opts == nil {
		opts = make(map[string]any)
	}
	opts["include_usage"] = true
	raw["stream_options"] = opts
	out, err := json.Marshal(raw)
	if err != nil {
		return body
	}
	return out
}

// serveStream pipes a streaming (SSE) LLM response directly to the client.
// Cache and response translation are skipped; chunks are flushed as received.
// The stream is scanned line-by-line so the last data payload can be inspected
// for usage counts (token rate limiting, metrics) without buffering the whole body.
// Backend retry is possible before w.WriteHeader; once the SSE stream starts,
// switching backends is no longer possible.
func (h *Handler) serveStream(w http.ResponseWriter, r *http.Request, def *service.Def, body []byte, prov provider.Provider, consumer, userType string, start time.Time) {
	if !h.checkAndWriteTokenLimit(w, r, def.Type, def.Model) {
		return
	}

	// When token rate limiting is active, inject stream_options.include_usage=true so
	// the backend includes a usage chunk in the stream (required for accurate counting).
	forwardBody := body
	if h.tokenLimiter != nil {
		forwardBody = injectStreamUsage(body)
	}

	backends := service.OrderedBackends(def.Backends)
	var resp *http.Response
	var lastErr string
	var winningBackendModel, winningBackendURL string
	for i, backend := range backends {
		effectiveModel := backend.Model
		if effectiveModel == "" {
			effectiveModel = def.BackendModel
		}
		upstreamBody := forwardBody
		if effectiveModel != "" {
			var rewriteErr error
			upstreamBody, rewriteErr = rewriteBodyModel(forwardBody, effectiveModel)
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
		otel.GetTextMapPropagator().Inject(r.Context(), propagation.HeaderCarrier(upstreamReq.Header))
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
		winningBackendURL = backend.URL
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
	metrics.ObserveWithExemplar(r.Context(), metrics.LLMRequestDuration.WithLabelValues(def.Type, def.Model, winningBackendModel, def.Provider, userType), time.Since(start).Seconds())

	// Pipe SSE lines to the client, flushing after each line.
	// We track the last non-[DONE] data payload to extract usage counts once
	// the stream is complete (token rate limiting, metrics).
	// We also accumulate delta content for output DLP scanning.
	flusher, canFlush := w.(http.Flusher)
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 1*1024*1024)
	var lastDataPayload string
	var deltaTexts []string
	for scanner.Scan() {
		line := scanner.Text()
		if _, writeErr := fmt.Fprintf(w, "%s\n", line); writeErr != nil {
			return // client disconnected
		}
		if canFlush {
			flusher.Flush()
		}
		if after, ok := strings.CutPrefix(line, "data: "); ok && after != "[DONE]" {
			lastDataPayload = after
			// Accumulate delta.content for output DLP scanning.
			if def.Guardrails.Output.Enabled {
				var chunk struct {
					Choices []struct {
						Delta struct {
							Content string `json:"content"`
						} `json:"delta"`
					} `json:"choices"`
				}
				if jsonErr := json.Unmarshal([]byte(after), &chunk); jsonErr == nil &&
					len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != "" {
					deltaTexts = append(deltaTexts, chunk.Choices[0].Delta.Content)
				}
			}
		}
	}

	var streamUsage *provider.Usage
	if lastDataPayload != "" {
		streamUsage = emitTokenMetrics(r.Context(), def, winningBackendModel, userType, []byte(lastDataPayload))
		if streamUsage != nil && h.tokenLimiter != nil {
			total := streamUsage.PromptTokens + streamUsage.CompletionTokens
			if total > 0 {
				tCtx := context.WithoutCancel(r.Context())
				_ = h.tokenLimiter.AddTokens(tCtx, r, def.Type, total)
				if def.Model != "" {
					_ = h.tokenLimiter.AddModelTokens(tCtx, r, def.Model, total)
				}
			}
		}
	}

	// ── Output DLP guardrails (streaming) ─────────────────────────────────────
	// Block and redact are not feasible on already-flushed SSE streams; we always
	// degrade to flag-only to at least emit a metric and log entry.
	if def.Guardrails.Output.Enabled && h.guard != nil && len(deltaTexts) > 0 {
		// Join the per-token deltas before scanning: a single delta rarely holds a
		// full match (e.g. an email streams as "bob","@","example",".","com"), so
		// scanning fragments individually would miss PII split across chunks.
		full := strings.Join(deltaTexts, "")
		if found := h.guard.ScanStrings([]string{full}, def.Guardrails.Output.Checks); len(found) > 0 {
			slog.WarnContext(r.Context(), "llm stream response flagged by output guardrails (block/redact degrade to flag for streams)",
				"service_type", def.Type, "model", def.Model, "consumer", consumer, "violations", found)
			metrics.GuardrailsTotal.WithLabelValues(def.Type, def.Model, "output", "flag", "flagged").Inc()
		}
	}

	h.auditLog(r.Context(), def, consumer, userType, winningBackendURL, winningBackendModel,
		resp.StatusCode, time.Since(start).Milliseconds(), true, body, streamUsage)
}

// auditLog emits a structured slog record for the LLM request when audit is enabled.
// usage may be nil (e.g. for streaming responses where tokens are not parsed).
func (h *Handler) auditLog(ctx context.Context, def *service.Def, consumer, userType, backendURL, backendModel string, status int, durationMs int64, stream bool, reqBody []byte, usage *provider.Usage) {
	if !h.audit.Enabled {
		return
	}
	args := []any{
		"service_type", def.Type,
		"model", def.Model,
		"backend_model", backendModel,
		"provider", def.Provider,
		"consumer", consumer,
		"user_type", userType,
		"status", status,
		"duration_ms", durationMs,
		"backend_url", backendURL,
		"stream", stream,
	}
	if usage != nil {
		args = append(args,
			"prompt_tokens", usage.PromptTokens,
			"completion_tokens", usage.CompletionTokens,
		)
	}
	if h.audit.Prompt && len(reqBody) > 0 {
		args = append(args, "prompt", string(reqBody))
	}
	slog.InfoContext(ctx, "llm request", args...)
}

func writeTokenLimitHeaders(w http.ResponseWriter, res ratelimit.CheckResult) {
	if res.Limit > 0 {
		w.Header().Set("X-TokenRateLimit-Limit", strconv.Itoa(res.Limit))
		w.Header().Set("X-TokenRateLimit-Remaining", "0")
		if res.ResetAfter > 0 {
			secs := strconv.Itoa(int(res.ResetAfter.Seconds()))
			w.Header().Set("X-TokenRateLimit-Reset", secs)
			w.Header().Set("Retry-After", secs)
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
