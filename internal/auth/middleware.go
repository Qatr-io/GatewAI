package auth

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
)

// Middleware enforces or attaches identity for each request.
//
// When a is nil or mode is "" the middleware is a no-op (legacy/backward-compatible).
//
// exempt is a slice of path prefixes that bypass authentication entirely
// (e.g. "/health", "/metrics", "/docs", "/openapi.yaml").
//
// bridgeConsumerHeader and bridgeUserTypeHeader are the header names that
// downstream handlers read (server.consumer_header / server.user_type_header).
// In oauth2 mode the middleware rewrites them from the validated Principal after
// stripping any inbound values (anti-spoofing). In proxy mode headers are left
// as-is (the upstream proxy already set them).
func Middleware(a Authenticator, mode string, exempt []string, bridgeConsumerHeader, bridgeUserTypeHeader string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		// No-op when auth is disabled.
		if a == nil || mode == "" {
			return next
		}

		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Exempt paths bypass auth entirely.
			for _, prefix := range exempt {
				if r.URL.Path == prefix || strings.HasPrefix(r.URL.Path, prefix+"/") {
					next.ServeHTTP(w, r)
					return
				}
			}

			p, err := a.Authenticate(r)

			switch mode {
			case "oauth2":
				if err != nil {
					if errors.Is(err, ErrNoCredentials) || errors.Is(err, ErrInvalidToken) {
						writeJSONError(w, http.StatusUnauthorized, err.Error())
						return
					}
					// Any other error (e.g. JWKS unreachable) — fail closed.
					slog.Warn("auth: authentication service temporarily unavailable", "error", err)
					writeJSONError(w, http.StatusServiceUnavailable, "authentication temporarily unavailable")
					return
				}

				// Strip inbound identity headers to prevent spoofing.
				r = r.Clone(r.Context())
				r.Header.Del("Authorization")
				if bridgeConsumerHeader != "" {
					r.Header.Del(bridgeConsumerHeader)
					if p.Consumer != "" {
						r.Header.Set(bridgeConsumerHeader, p.Consumer)
					}
				}
				if bridgeUserTypeHeader != "" {
					r.Header.Del(bridgeUserTypeHeader)
					if p.UserType != "" {
						r.Header.Set(bridgeUserTypeHeader, p.UserType)
					}
				}

				ctx := WithPrincipal(r.Context(), p)
				next.ServeHTTP(w, r.WithContext(ctx))

			case "proxy":
				// proxy mode never hard-fails; attach whatever principal was resolved.
				if p != nil {
					ctx := WithPrincipal(r.Context(), p)
					r = r.WithContext(ctx)
				}
				next.ServeHTTP(w, r)
			}
		})
	}
}

// writeJSONError writes a JSON error response without importing the handler package.
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body, _ := json.Marshal(map[string]string{"error": msg})
	_, _ = w.Write(body)
}
