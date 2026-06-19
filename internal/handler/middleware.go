package handler

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"go.opentelemetry.io/otel"
)

// otelResponseWriter wraps http.ResponseWriter to capture the status code.
type otelResponseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *otelResponseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

// OtelMiddleware starts a server span for every request.
// It extracts W3C traceparent from incoming headers, creates a child span,
// and updates the span name with the chi route pattern after routing.
// Paths listed in skip are passed through without creating a span (e.g. /health, /metrics).
func OtelMiddleware(tracer trace.Tracer, skip ...string) func(http.Handler) http.Handler {
	skipSet := make(map[string]struct{}, len(skip))
	for _, p := range skip {
		skipSet[p] = struct{}{}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, ok := skipSet[r.URL.Path]; ok {
				next.ServeHTTP(w, r)
				return
			}

			// Extract parent context from incoming W3C headers (if any).
			ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))

			ctx, span := tracer.Start(ctx, r.Method,
				trace.WithSpanKind(trace.SpanKindServer),
				trace.WithAttributes(
					attribute.String("http.method", r.Method),
					attribute.String("net.host.name", r.Host),
				),
			)
			defer span.End()

			rw := &otelResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}
			next.ServeHTTP(rw, r.WithContext(ctx))

			// Route pattern is available after chi routing completes.
			if rctx := chi.RouteContext(r.Context()); rctx != nil {
				if route := rctx.RoutePattern(); route != "" {
					span.SetName(r.Method + " " + route)
					span.SetAttributes(attribute.String("http.route", route))
				}
			}
			span.SetAttributes(attribute.Int("http.status_code", rw.statusCode))
			if rw.statusCode >= 500 {
				span.SetStatus(codes.Error, "server error")
			}
		})
	}
}

// StructuredLogger returns a chi-compatible middleware that emits one
// structured slog entry per request (JSON in production, text in dev).
func StructuredLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			start := time.Now()

			next.ServeHTTP(ww, r)

			logger.InfoContext(r.Context(), "request",
				"method",      r.Method,
				"path",        r.URL.Path,
				"status",      ww.Status(),
				"bytes",       ww.BytesWritten(),
				"duration_ms", time.Since(start).Milliseconds(),
				"request_id",  middleware.GetReqID(r.Context()),
				"remote",      r.RemoteAddr,
			)
		})
	}
}
