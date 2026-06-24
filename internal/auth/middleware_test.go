package auth_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"gatewai/gateway/internal/auth"
)

// stubAuthenticator is a test double that returns a fixed (principal, error) pair.
type stubAuthenticator struct {
	principal *auth.Principal
	err       error
}

func (s *stubAuthenticator) Authenticate(_ *http.Request) (*auth.Principal, error) {
	return s.principal, s.err
}

// nextCalled records whether the inner handler was invoked, and captures the request.
type nextCapture struct {
	called       bool
	lastReq      *http.Request
	principal    *auth.Principal
	hadPrincipal bool
}

func (n *nextCapture) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.called = true
		n.lastReq = r
		n.principal, n.hadPrincipal = auth.FromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
}

// helper: run a single request through the middleware and return the recorder.
func runMiddleware(t *testing.T, a auth.Authenticator, mode string, exempt []string,
	bridgeConsumer, bridgeUserType string, req *http.Request) (*httptest.ResponseRecorder, *nextCapture) {
	t.Helper()
	cap := &nextCapture{}
	mw := auth.Middleware(a, mode, exempt, bridgeConsumer, bridgeUserType)
	handler := mw(cap.Handler())
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr, cap
}

// ── nil authenticator / mode "" ─────────────────────────────────────────────

func TestMiddleware_NilAuthenticator_PassesThrough(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/jobs", nil)
	rr, cap := runMiddleware(t, nil, "", nil, "", "", req)
	if rr.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rr.Code)
	}
	if !cap.called {
		t.Error("inner handler should have been called")
	}
}

func TestMiddleware_EmptyMode_PassesThrough(t *testing.T) {
	stub := &stubAuthenticator{principal: &auth.Principal{Consumer: "alice", Authenticated: true}}
	req := httptest.NewRequest(http.MethodGet, "/jobs", nil)
	rr, cap := runMiddleware(t, stub, "", nil, "", "", req)
	if rr.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rr.Code)
	}
	if !cap.called {
		t.Error("inner handler should have been called")
	}
}

// ── Exempt paths ─────────────────────────────────────────────────────────────

func TestMiddleware_ExemptPath_BypassesAuth(t *testing.T) {
	// A stub that always returns ErrNoCredentials — auth would block if called.
	stub := &stubAuthenticator{err: auth.ErrNoCredentials}
	exempt := []string{"/health", "/metrics", "/docs", "/openapi.yaml"}

	for _, path := range []string{"/health", "/metrics", "/docs", "/openapi.yaml", "/docs/spec/llm/gpt-4o"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rr, cap := runMiddleware(t, stub, "oauth2", exempt, "", "", req)
		if rr.Code != http.StatusOK {
			t.Errorf("path %q: expected 200 (exempt), got %d", path, rr.Code)
		}
		if !cap.called {
			t.Errorf("path %q: inner handler should have been called for exempt path", path)
		}
	}
}

// ── oauth2 mode ─────────────────────────────────────────────────────────────

func TestMiddleware_OAuth2_MissingCredentials_Returns401(t *testing.T) {
	stub := &stubAuthenticator{err: auth.ErrNoCredentials}
	req := httptest.NewRequest(http.MethodPost, "/jobs/llm", nil)
	rr, cap := runMiddleware(t, stub, "oauth2", nil, "", "", req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rr.Code)
	}
	if cap.called {
		t.Error("inner handler must NOT be called on 401")
	}
}

func TestMiddleware_OAuth2_InvalidToken_Returns401(t *testing.T) {
	stub := &stubAuthenticator{err: auth.ErrInvalidToken}
	req := httptest.NewRequest(http.MethodPost, "/jobs/llm", nil)
	req.Header.Set("Authorization", "Bearer bad.token.here")
	rr, cap := runMiddleware(t, stub, "oauth2", nil, "", "", req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rr.Code)
	}
	if cap.called {
		t.Error("inner handler must NOT be called on 401")
	}
}

func TestMiddleware_OAuth2_WrappedInvalidToken_Returns401(t *testing.T) {
	// ErrInvalidToken may be wrapped (fmt.Errorf("%w: ...", ErrInvalidToken)).
	wrapped := errors.Join(auth.ErrInvalidToken, errors.New("token expired"))
	stub := &stubAuthenticator{err: wrapped}
	req := httptest.NewRequest(http.MethodPost, "/jobs/llm", nil)
	rr, cap := runMiddleware(t, stub, "oauth2", nil, "", "", req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for wrapped ErrInvalidToken, got %d", rr.Code)
	}
	if cap.called {
		t.Error("inner handler must NOT be called")
	}
}

func TestMiddleware_OAuth2_OtherError_Returns503(t *testing.T) {
	stub := &stubAuthenticator{err: errors.New("JWKS endpoint unreachable")}
	req := httptest.NewRequest(http.MethodPost, "/jobs/llm", nil)
	rr, cap := runMiddleware(t, stub, "oauth2", nil, "", "", req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", rr.Code)
	}
	if cap.called {
		t.Error("inner handler must NOT be called on 503")
	}
}

func TestMiddleware_OAuth2_Success_AttachesPrincipalAndSetsHeaders(t *testing.T) {
	p := &auth.Principal{
		Subject:       "sub-123",
		Consumer:      "alice",
		UserType:      "user",
		Authenticated: true,
	}
	stub := &stubAuthenticator{principal: p}

	req := httptest.NewRequest(http.MethodPost, "/jobs/llm", nil)
	req.Header.Set("Authorization", "Bearer valid.token")
	// Pre-populate bridge headers to verify they get replaced, not leaked.
	req.Header.Set("X-Consumer-Username", "attacker")
	req.Header.Set("X-User-Type", "admin")

	rr, cap := runMiddleware(t, stub, "oauth2", nil, "X-Consumer-Username", "X-User-Type", req)
	if rr.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rr.Code)
	}
	if !cap.called {
		t.Fatal("inner handler must be called on success")
	}

	// Principal must be in context.
	if !cap.hadPrincipal {
		t.Error("principal must be stored in context")
	}
	if cap.principal.Consumer != "alice" {
		t.Errorf("expected Consumer=alice, got %q", cap.principal.Consumer)
	}

	// Bridge headers must be rewritten to validated values.
	if got := cap.lastReq.Header.Get("X-Consumer-Username"); got != "alice" {
		t.Errorf("X-Consumer-Username: expected alice, got %q", got)
	}
	if got := cap.lastReq.Header.Get("X-User-Type"); got != "user" {
		t.Errorf("X-User-Type: expected user, got %q", got)
	}

	// Authorization header must be stripped.
	if got := cap.lastReq.Header.Get("Authorization"); got != "" {
		t.Errorf("Authorization header must be stripped, got %q", got)
	}
}

func TestMiddleware_OAuth2_Success_StripAuthorizationHeader(t *testing.T) {
	p := &auth.Principal{Subject: "sub", Consumer: "bob", Authenticated: true}
	stub := &stubAuthenticator{principal: p}

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer should.be.stripped")

	_, cap := runMiddleware(t, stub, "oauth2", nil, "", "", req)
	if cap.lastReq.Header.Get("Authorization") != "" {
		t.Error("Authorization header must be stripped in oauth2 mode")
	}
}

func TestMiddleware_OAuth2_Success_EmptyConsumer_HeaderNotSet(t *testing.T) {
	// When the principal has no Consumer, the bridge header should be cleared
	// rather than set to an empty string.
	p := &auth.Principal{Subject: "sub", Authenticated: true} // Consumer is ""
	stub := &stubAuthenticator{principal: p}

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("X-Consumer-Username", "old-value")

	_, cap := runMiddleware(t, stub, "oauth2", nil, "X-Consumer-Username", "", req)
	if got := cap.lastReq.Header.Get("X-Consumer-Username"); got != "" {
		t.Errorf("empty Consumer: X-Consumer-Username should be cleared, got %q", got)
	}
}

// ── proxy mode ───────────────────────────────────────────────────────────────

func TestMiddleware_Proxy_AttachesPrincipal(t *testing.T) {
	p := &auth.Principal{Consumer: "svc-account", UserType: "sa", Authenticated: true}
	stub := &stubAuthenticator{principal: p}

	req := httptest.NewRequest(http.MethodPost, "/jobs/transcription", nil)
	req.Header.Set("X-Consumer-Username", "svc-account")

	rr, cap := runMiddleware(t, stub, "proxy", nil, "X-Consumer-Username", "X-User-Type", req)
	if rr.Code != http.StatusOK {
		t.Errorf("proxy mode: expected 200, got %d", rr.Code)
	}
	if !cap.hadPrincipal {
		t.Error("principal must be attached in proxy mode")
	}
	if cap.principal.Consumer != "svc-account" {
		t.Errorf("expected Consumer=svc-account, got %q", cap.principal.Consumer)
	}
}

func TestMiddleware_Proxy_DoesNotRewriteHeaders(t *testing.T) {
	p := &auth.Principal{Consumer: "proxy-consumer", Authenticated: true}
	stub := &stubAuthenticator{principal: p}

	req := httptest.NewRequest(http.MethodPost, "/jobs/llm", nil)
	req.Header.Set("X-Consumer-Username", "original-value")
	req.Header.Set("Authorization", "Bearer some-token")

	_, cap := runMiddleware(t, stub, "proxy", nil, "X-Consumer-Username", "X-User-Type", req)

	// In proxy mode, headers are left as-is — the upstream proxy already set them.
	if got := cap.lastReq.Header.Get("X-Consumer-Username"); got != "original-value" {
		t.Errorf("proxy mode must not rewrite X-Consumer-Username: got %q", got)
	}
	// Authorization header is NOT stripped in proxy mode.
	if got := cap.lastReq.Header.Get("Authorization"); got != "Bearer some-token" {
		t.Errorf("proxy mode must not strip Authorization header: got %q", got)
	}
}

func TestMiddleware_Proxy_UnauthenticatedPrincipalAttached(t *testing.T) {
	// HeaderAuthenticator returns Authenticated=false when no consumer header is present.
	p := &auth.Principal{Authenticated: false}
	stub := &stubAuthenticator{principal: p, err: nil}

	req := httptest.NewRequest(http.MethodGet, "/jobs", nil)
	rr, cap := runMiddleware(t, stub, "proxy", nil, "", "", req)
	if rr.Code != http.StatusOK {
		t.Errorf("proxy mode never blocks: expected 200, got %d", rr.Code)
	}
	if !cap.hadPrincipal {
		t.Error("even an unauthenticated principal should be attached in proxy mode")
	}
}
