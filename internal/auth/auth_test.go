package auth_test

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"

	"gatewai/gateway/internal/auth"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// generateRSAKey creates a fresh 2048-bit RSA key for tests.
func generateRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	return key
}

// makeJWKSJSON encodes the public key of priv as a minimal JWK Set JSON blob.
func makeJWKSJSON(t *testing.T, kid string, priv *rsa.PrivateKey) json.RawMessage {
	t.Helper()
	pub := &priv.PublicKey
	n := base64.RawURLEncoding.EncodeToString(pub.N.Bytes())
	e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes())
	raw, err := json.Marshal(map[string]any{
		"keys": []map[string]any{
			{
				"kty": "RSA",
				"kid": kid,
				"alg": "RS256",
				"use": "sig",
				"n":   n,
				"e":   e,
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal JWKS: %v", err)
	}
	return raw
}

// makeKeyfunc builds a jwt.Keyfunc from a raw JWK Set JSON blob via keyfunc/v3.
func makeKeyfunc(t *testing.T, jwksJSON json.RawMessage) jwt.Keyfunc {
	t.Helper()
	kfSet, err := keyfunc.NewJWKSetJSON(jwksJSON)
	if err != nil {
		t.Fatalf("keyfunc.NewJWKSetJSON: %v", err)
	}
	return kfSet.Keyfunc
}

// tokenClaims is a convenience type for minting test tokens.
type tokenClaims struct {
	jwt.RegisteredClaims
	PreferredUsername string   `json:"preferred_username,omitempty"`
	Scope             string   `json:"scope,omitempty"`
	Groups            []string `json:"groups,omitempty"`
	Roles             []string `json:"roles,omitempty"`
}

// mintToken signs a JWT with priv.  Pass kid="" to omit the header field.
func mintToken(t *testing.T, kid string, priv *rsa.PrivateKey, claims tokenClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	if kid != "" {
		tok.Header["kid"] = kid
	}
	raw, err := tok.SignedString(priv)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return raw
}

// requestWithBearer creates a GET request with the given bearer token.
func requestWithBearer(token string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return r
}

// defaultClaims returns a valid set of claims for the happy-path test.
func defaultClaims(issuer string, audiences []string) tokenClaims {
	return tokenClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    issuer,
			Subject:   "u-1",
			Audience:  jwt.ClaimStrings(audiences),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
		PreferredUsername: "alice",
		Scope:             "a b",
		Groups:            []string{"g1"},
		Roles:             []string{"r1"},
	}
}

// ---------------------------------------------------------------------------
// OAuth2Authenticator tests
// ---------------------------------------------------------------------------

const (
	testIssuer = "https://idp.example.com"
	testKID    = "test-key-1"
)

func setupAuth(t *testing.T) (*auth.OAuth2Authenticator, *rsa.PrivateKey) {
	t.Helper()
	priv := generateRSAKey(t)
	jwksJSON := makeJWKSJSON(t, testKID, priv)
	kf := makeKeyfunc(t, jwksJSON)

	a := auth.NewOAuth2AuthenticatorWithKeyfunc(auth.OAuth2Config{
		Issuer:    testIssuer,
		Audiences: []string{"my-gateway"},
		Claims: auth.ClaimMap{
			Consumer: "preferred_username",
			Groups:   "groups",
			Roles:    "roles",
		},
	}, kf)
	return a, priv
}

// TestOAuth2_HappyPath verifies that a fully valid token is accepted and all
// Principal fields are populated correctly.
func TestOAuth2_HappyPath(t *testing.T) {
	a, priv := setupAuth(t)
	claims := defaultClaims(testIssuer, []string{"my-gateway"})
	raw := mintToken(t, testKID, priv, claims)

	p, err := a.Authenticate(requestWithBearer(raw))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p == nil {
		t.Fatal("expected non-nil Principal")
	}
	if !p.Authenticated {
		t.Error("Authenticated should be true")
	}
	if p.Subject != "u-1" {
		t.Errorf("Subject = %q, want %q", p.Subject, "u-1")
	}
	if p.Consumer != "alice" {
		t.Errorf("Consumer = %q, want %q", p.Consumer, "alice")
	}
	if len(p.Scopes) != 2 || p.Scopes[0] != "a" || p.Scopes[1] != "b" {
		t.Errorf("Scopes = %v, want [a b]", p.Scopes)
	}
	if len(p.Groups) != 1 || p.Groups[0] != "g1" {
		t.Errorf("Groups = %v, want [g1]", p.Groups)
	}
	if len(p.Roles) != 1 || p.Roles[0] != "r1" {
		t.Errorf("Roles = %v, want [r1]", p.Roles)
	}
}

// TestOAuth2_NoCredentials ensures that a missing Authorization header returns
// ErrNoCredentials.
func TestOAuth2_NoCredentials(t *testing.T) {
	a, _ := setupAuth(t)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	_, err := a.Authenticate(r)
	if !errors.Is(err, auth.ErrNoCredentials) {
		t.Errorf("err = %v, want ErrNoCredentials", err)
	}
}

// TestOAuth2_BlankBearer ensures a blank bearer value returns ErrNoCredentials.
func TestOAuth2_BlankBearer(t *testing.T) {
	a, _ := setupAuth(t)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer ")
	_, err := a.Authenticate(r)
	if !errors.Is(err, auth.ErrNoCredentials) {
		t.Errorf("err = %v, want ErrNoCredentials", err)
	}
}

// TestOAuth2_ExpiredToken verifies that an expired token returns ErrInvalidToken.
func TestOAuth2_ExpiredToken(t *testing.T) {
	a, priv := setupAuth(t)
	claims := tokenClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    testIssuer,
			Subject:   "u-1",
			Audience:  jwt.ClaimStrings([]string{"my-gateway"}),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(-time.Hour)), // in the past
			IssuedAt:  jwt.NewNumericDate(time.Now().Add(-2 * time.Hour)),
		},
	}
	raw := mintToken(t, testKID, priv, claims)
	_, err := a.Authenticate(requestWithBearer(raw))
	if !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("err = %v, want ErrInvalidToken", err)
	}
}

// TestOAuth2_WrongIssuer verifies that a token with a different issuer is rejected.
func TestOAuth2_WrongIssuer(t *testing.T) {
	a, priv := setupAuth(t)
	claims := defaultClaims("https://evil.example.com", []string{"my-gateway"})
	raw := mintToken(t, testKID, priv, claims)
	_, err := a.Authenticate(requestWithBearer(raw))
	if !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("err = %v, want ErrInvalidToken", err)
	}
}

// TestOAuth2_AudienceMismatch verifies that a token without a required audience
// is rejected.
func TestOAuth2_AudienceMismatch(t *testing.T) {
	a, priv := setupAuth(t)
	claims := defaultClaims(testIssuer, []string{"other-service"})
	raw := mintToken(t, testKID, priv, claims)
	_, err := a.Authenticate(requestWithBearer(raw))
	if !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("err = %v, want ErrInvalidToken", err)
	}
}

// TestOAuth2_WrongKey verifies that a token signed by a different key is rejected.
func TestOAuth2_WrongKey(t *testing.T) {
	a, _ := setupAuth(t) // authenticator has key A
	// Mint a token with an entirely different key.
	otherPriv := generateRSAKey(t)
	claims := defaultClaims(testIssuer, []string{"my-gateway"})
	raw := mintToken(t, testKID, otherPriv, claims)
	_, err := a.Authenticate(requestWithBearer(raw))
	if !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("err = %v, want ErrInvalidToken", err)
	}
}

// TestOAuth2_AudienceSkippedWhenEmpty verifies that when cfg.Audiences is nil,
// no audience check is performed.
func TestOAuth2_AudienceSkippedWhenEmpty(t *testing.T) {
	priv := generateRSAKey(t)
	jwksJSON := makeJWKSJSON(t, testKID, priv)
	kf := makeKeyfunc(t, jwksJSON)

	a := auth.NewOAuth2AuthenticatorWithKeyfunc(auth.OAuth2Config{
		Issuer:    testIssuer,
		Audiences: nil, // no audience check
		Claims:    auth.ClaimMap{},
	}, kf)

	// Token without any aud claim should still be accepted.
	claims := tokenClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    testIssuer,
			Subject:   "u-noaud",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}
	raw := mintToken(t, testKID, priv, claims)
	p, err := a.Authenticate(requestWithBearer(raw))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Subject != "u-noaud" {
		t.Errorf("Subject = %q, want %q", p.Subject, "u-noaud")
	}
}

// ---------------------------------------------------------------------------
// Claim mapping edge-case tests
// ---------------------------------------------------------------------------

// TestClaimMapping_ScopeAsString verifies that a space-delimited scope string
// is split into individual tokens.
func TestClaimMapping_ScopeAsString(t *testing.T) {
	a, priv := setupAuth(t)
	claims := defaultClaims(testIssuer, []string{"my-gateway"})
	claims.Scope = "read write admin"
	raw := mintToken(t, testKID, priv, claims)
	p, err := a.Authenticate(requestWithBearer(raw))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"read", "write", "admin"}
	if len(p.Scopes) != len(want) {
		t.Fatalf("Scopes = %v, want %v", p.Scopes, want)
	}
	for i, s := range want {
		if p.Scopes[i] != s {
			t.Errorf("Scopes[%d] = %q, want %q", i, p.Scopes[i], s)
		}
	}
}

// nestedClaims is like tokenClaims but embeds roles under realm_access.roles.
type nestedClaims struct {
	jwt.RegisteredClaims
	RealmAccess map[string]any `json:"realm_access,omitempty"`
}

// TestClaimMapping_NestedRoles verifies dotted-path resolution ("realm_access.roles").
func TestClaimMapping_NestedRoles(t *testing.T) {
	priv := generateRSAKey(t)
	jwksJSON := makeJWKSJSON(t, testKID, priv)
	kf := makeKeyfunc(t, jwksJSON)

	a := auth.NewOAuth2AuthenticatorWithKeyfunc(auth.OAuth2Config{
		Issuer:    testIssuer,
		Audiences: []string{"my-gateway"},
		Claims: auth.ClaimMap{
			Roles: "realm_access.roles",
		},
	}, kf)

	claims := nestedClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    testIssuer,
			Subject:   "u-nested",
			Audience:  jwt.ClaimStrings([]string{"my-gateway"}),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
		RealmAccess: map[string]any{
			"roles": []any{"admin", "viewer"},
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = testKID
	raw, err := tok.SignedString(priv)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}

	p, authErr := a.Authenticate(requestWithBearer(raw))
	if authErr != nil {
		t.Fatalf("unexpected error: %v", authErr)
	}
	if len(p.Roles) != 2 || p.Roles[0] != "admin" || p.Roles[1] != "viewer" {
		t.Errorf("Roles = %v, want [admin viewer]", p.Roles)
	}
}

// TestClaimMapping_GroupsAsArray verifies that a []any groups claim is coerced
// to []string correctly.
func TestClaimMapping_GroupsAsArray(t *testing.T) {
	a, priv := setupAuth(t)
	claims := defaultClaims(testIssuer, []string{"my-gateway"})
	claims.Groups = []string{"eng", "ops", "platform"}
	raw := mintToken(t, testKID, priv, claims)
	p, err := a.Authenticate(requestWithBearer(raw))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(p.Groups) != 3 {
		t.Errorf("Groups = %v, want [eng ops platform]", p.Groups)
	}
}

// ---------------------------------------------------------------------------
// Context helpers
// ---------------------------------------------------------------------------

func TestContextRoundtrip(t *testing.T) {
	want := &auth.Principal{Subject: "abc", Authenticated: true}
	ctx := auth.WithPrincipal(t.Context(), want)
	got, ok := auth.FromContext(ctx)
	if !ok {
		t.Fatal("FromContext: not found")
	}
	if got != want {
		t.Errorf("FromContext returned %p, want %p", got, want)
	}
}

func TestContextMissing(t *testing.T) {
	_, ok := auth.FromContext(t.Context())
	if ok {
		t.Error("expected ok=false for empty context")
	}
}

// ---------------------------------------------------------------------------
// HeaderAuthenticator tests
// ---------------------------------------------------------------------------

func TestHeaderAuth_Populated(t *testing.T) {
	ha := auth.NewHeaderAuthenticator(auth.HeaderConfig{
		ConsumerHeader: "X-Consumer-Username",
		UserTypeHeader: "X-User-Type",
		GroupsHeader:   "X-Groups",
		RolesHeader:    "X-Roles",
		ScopesHeader:   "X-Scopes",
	})

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-Consumer-Username", "bob")
	r.Header.Set("X-User-Type", "sa")
	r.Header.Set("X-Groups", "dev, ops, sre")
	r.Header.Set("X-Roles", "reader writer")
	r.Header.Set("X-Scopes", "read,write")

	p, err := ha.Authenticate(r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !p.Authenticated {
		t.Error("Authenticated should be true")
	}
	if p.Subject != "bob" {
		t.Errorf("Subject = %q, want %q", p.Subject, "bob")
	}
	if p.Consumer != "bob" {
		t.Errorf("Consumer = %q, want %q", p.Consumer, "bob")
	}
	if p.UserType != "sa" {
		t.Errorf("UserType = %q, want %q", p.UserType, "sa")
	}
	if len(p.Groups) != 3 {
		t.Errorf("Groups = %v, want 3 elements", p.Groups)
	}
	if len(p.Roles) != 2 {
		t.Errorf("Roles = %v, want 2 elements", p.Roles)
	}
	if len(p.Scopes) != 2 {
		t.Errorf("Scopes = %v, want 2 elements", p.Scopes)
	}
}

// TestHeaderAuth_MissingConsumer verifies that when the consumer header is absent,
// Authenticated=false is returned with no error.
func TestHeaderAuth_MissingConsumer(t *testing.T) {
	ha := auth.NewHeaderAuthenticator(auth.HeaderConfig{
		ConsumerHeader: "X-Consumer-Username",
		UserTypeHeader: "X-User-Type",
	})

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	// Consumer header intentionally not set.
	r.Header.Set("X-User-Type", "user")

	p, err := ha.Authenticate(r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Authenticated {
		t.Error("Authenticated should be false when consumer header is missing")
	}
	if p.Subject != "" {
		t.Errorf("Subject = %q, want empty", p.Subject)
	}
}

// TestHeaderAuth_EmptyConfig verifies that an empty HeaderConfig never panics
// and returns an unauthenticated Principal.
func TestHeaderAuth_EmptyConfig(t *testing.T) {
	ha := auth.NewHeaderAuthenticator(auth.HeaderConfig{})
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	p, err := ha.Authenticate(r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Authenticated {
		t.Error("Authenticated should be false for empty config")
	}
}

// TestHeaderAuth_GroupsSplitVariants verifies that groups split on both commas
// and whitespace, and that extra spaces are ignored.
func TestHeaderAuth_GroupsSplitVariants(t *testing.T) {
	ha := auth.NewHeaderAuthenticator(auth.HeaderConfig{
		ConsumerHeader: "X-Consumer",
		GroupsHeader:   "X-Groups",
	})

	cases := []struct {
		header string
		want   []string
	}{
		{"a,b,c", []string{"a", "b", "c"}},
		{"a b c", []string{"a", "b", "c"}},
		{"a,  b ,c", []string{"a", "b", "c"}},
		{"single", []string{"single"}},
		{"", nil},
	}

	for _, tc := range cases {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("X-Consumer", "testuser")
		if tc.header != "" {
			r.Header.Set("X-Groups", tc.header)
		}
		p, err := ha.Authenticate(r)
		if err != nil {
			t.Fatalf("header=%q: unexpected error: %v", tc.header, err)
		}
		if len(p.Groups) != len(tc.want) {
			t.Errorf("header=%q: Groups=%v, want %v", tc.header, p.Groups, tc.want)
			continue
		}
		for i, g := range tc.want {
			if p.Groups[i] != g {
				t.Errorf("header=%q: Groups[%d]=%q, want %q", tc.header, i, p.Groups[i], g)
			}
		}
	}
}
