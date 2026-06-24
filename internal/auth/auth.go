// Package auth resolves a request's identity into a Principal.
//
// Two authenticators are provided:
//   - OAuth2Authenticator: validates JWT access tokens via JWKS (RS256/ES256 family)
//     or opaque tokens via RFC 7662 token introspection, or both (auto mode).
//   - HeaderAuthenticator: trusts identity headers injected by an upstream proxy (APISIX/OPA).
//
// Usage pattern — call Authenticate, then store the result with WithPrincipal:
//
//	p, err := authenticator.Authenticate(r)
//	if errors.Is(err, auth.ErrNoCredentials) { ... }
//	if errors.Is(err, auth.ErrInvalidToken)  { ... }
//	ctx := auth.WithPrincipal(r.Context(), p)
package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"
)

// Principal is the resolved identity for a request.
type Principal struct {
	Subject       string // stable id (sub claim, or consumer in header mode)
	Consumer      string // human-readable name (preferred_username / consumer header)
	Groups        []string
	Roles         []string
	Scopes        []string
	UserType      string
	Authenticated bool
}

// contextKey is the unexported type used for context values in this package.
type contextKey struct{}

// WithPrincipal returns a copy of ctx with p stored as the current Principal.
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, contextKey{}, p)
}

// FromContext retrieves the Principal stored by WithPrincipal.
// Returns (nil, false) if no Principal is present.
func FromContext(ctx context.Context) (*Principal, bool) {
	p, ok := ctx.Value(contextKey{}).(*Principal)
	return p, ok
}

// Authenticator resolves a Principal from a request.
type Authenticator interface {
	Authenticate(r *http.Request) (*Principal, error)
}

// Sentinel errors — use errors.Is at call sites.
var (
	// ErrNoCredentials is returned when the request carries no recognisable credentials.
	ErrNoCredentials = errors.New("auth: no credentials presented")
	// ErrInvalidToken is returned when credentials were presented but failed validation.
	ErrInvalidToken = errors.New("auth: invalid token")
)

// ---------------------------------------------------------------------------
// ClaimMap & OAuth2Config
// ---------------------------------------------------------------------------

// ClaimMap maps Principal fields to JWT claim names, supporting dotted paths
// for nested claims (e.g. "realm_access.roles").
type ClaimMap struct {
	Subject  string // default "sub"
	Consumer string // e.g. "preferred_username"
	Scopes   string // default "scope"
	Groups   string // e.g. "groups"
	Roles    string // e.g. "roles"
}

// OAuth2Config holds the configuration for the OAuth2 resource-server validator.
type OAuth2Config struct {
	Issuer        string   // required; expected "iss"
	JWKSURL       string   // optional; if empty, discovered via {Issuer}/.well-known/openid-configuration
	Audiences     []string // accepted "aud" values (at least one must match); empty → skip aud check
	Claims        ClaimMap
	Validation    string // "" | "auto" | "jwt" | "introspection"
	Introspection *IntrospectionConfig
}

// IntrospectionConfig holds configuration for RFC 7662 token introspection.
type IntrospectionConfig struct {
	Endpoint     string
	ClientID     string
	ClientSecret string
	CacheTTL     time.Duration // default 60s if zero
}

// claimDefaults fills in ClaimMap zero values with sensible defaults.
func claimDefaults(cm ClaimMap) ClaimMap {
	if cm.Subject == "" {
		cm.Subject = "sub"
	}
	if cm.Scopes == "" {
		cm.Scopes = "scope"
	}
	return cm
}

// ---------------------------------------------------------------------------
// OAuth2Authenticator
// ---------------------------------------------------------------------------

// OAuth2Authenticator validates Bearer access tokens using JWKS and/or
// RFC 7662 token introspection, depending on the configured strategy.
type OAuth2Authenticator struct {
	cfg          OAuth2Config
	kf           jwt.Keyfunc // nil when strategy == introspection-only
	strategy     string      // "jwt" | "introspection" | "auto"
	introspector *tokenIntrospector
}

// NewOAuth2Authenticator creates an OAuth2Authenticator with auto-refreshing JWKS
// and/or a token introspector, depending on the Validation field of cfg.
// If cfg.JWKSURL is empty the JWKS URI is discovered from the issuer's
// OpenID Connect metadata endpoint.
func NewOAuth2Authenticator(ctx context.Context, cfg OAuth2Config) (*OAuth2Authenticator, error) {
	// Determine strategy.
	strategy := cfg.Validation
	if strategy == "" {
		strategy = "auto"
	}

	a := &OAuth2Authenticator{
		cfg:      cfg,
		strategy: strategy,
	}

	// Build introspector when needed.
	needsIntrospection := strategy == "introspection" ||
		(strategy == "auto" && cfg.Introspection != nil)

	if needsIntrospection {
		icfg := *cfg.Introspection // copy

		// Resolve introspection endpoint.
		if icfg.Endpoint == "" {
			// Discover from OIDC metadata.
			_, introspectionEndpoint, err := discoverOIDCEndpoints(cfg.Issuer)
			if err != nil {
				if strategy == "introspection" {
					return nil, fmt.Errorf("auth: introspection endpoint discovery for issuer %q: %w", cfg.Issuer, err)
				}
				// auto + discovery failure: fall back to JWT-only.
			} else {
				icfg.Endpoint = introspectionEndpoint
			}
		}

		if icfg.Endpoint != "" {
			if icfg.CacheTTL == 0 {
				icfg.CacheTTL = 60 * time.Second
			}
			a.introspector = &tokenIntrospector{
				cfg:    icfg,
				client: &http.Client{Timeout: 5 * time.Second},
				cache:  make(map[[32]byte]introspectionCacheEntry),
				cm:     claimDefaults(cfg.Claims),
			}
		} else if strategy == "introspection" {
			return nil, fmt.Errorf("auth: introspection endpoint is required (set introspection.endpoint or issuer for discovery)")
		}
	}

	// Build JWKS keyfunc when needed.
	needsJWT := strategy == "jwt" || strategy == "auto"
	if needsJWT {
		jwksURL := cfg.JWKSURL
		if jwksURL == "" {
			discovered, _, err := discoverOIDCEndpoints(cfg.Issuer)
			if err != nil {
				return nil, fmt.Errorf("auth: JWKS discovery for issuer %q: %w", cfg.Issuer, err)
			}
			jwksURL = discovered
		}

		kfSet, err := keyfunc.NewDefaultCtx(ctx, []string{jwksURL})
		if err != nil {
			return nil, fmt.Errorf("auth: keyfunc init: %w", err)
		}
		a.kf = kfSet.Keyfunc
	}

	return a, nil
}

// NewOAuth2AuthenticatorWithKeyfunc creates an OAuth2Authenticator with an
// injected jwt.Keyfunc. Intended for unit tests that provide keys without HTTP.
// Creates a JWT-strategy authenticator.
func NewOAuth2AuthenticatorWithKeyfunc(cfg OAuth2Config, kf jwt.Keyfunc) *OAuth2Authenticator {
	strategy := cfg.Validation
	if strategy == "" {
		strategy = "auto"
	}
	return &OAuth2Authenticator{cfg: cfg, kf: kf, strategy: strategy}
}

// NewIntrospectionAuthenticator creates an OAuth2Authenticator that validates
// tokens exclusively via RFC 7662 token introspection. Intended for tests and
// deployments that only use opaque tokens.
func NewIntrospectionAuthenticator(cfg OAuth2Config, icfg IntrospectionConfig) *OAuth2Authenticator {
	if icfg.CacheTTL == 0 {
		icfg.CacheTTL = 60 * time.Second
	}
	intr := &tokenIntrospector{
		cfg:    icfg,
		client: &http.Client{Timeout: 5 * time.Second},
		cache:  make(map[[32]byte]introspectionCacheEntry),
		cm:     claimDefaults(cfg.Claims),
	}
	return &OAuth2Authenticator{
		cfg:          cfg,
		strategy:     "introspection",
		introspector: intr,
	}
}

// NewAutoAuthenticatorWithKeyfunc creates an OAuth2Authenticator with auto strategy,
// accepting both a jwt.Keyfunc (for JWTs) and an IntrospectionConfig (for opaque tokens).
// Intended for unit tests.
func NewAutoAuthenticatorWithKeyfunc(cfg OAuth2Config, kf jwt.Keyfunc, icfg IntrospectionConfig) *OAuth2Authenticator {
	if icfg.CacheTTL == 0 {
		icfg.CacheTTL = 60 * time.Second
	}
	intr := &tokenIntrospector{
		cfg:    icfg,
		client: &http.Client{Timeout: 5 * time.Second},
		cache:  make(map[[32]byte]introspectionCacheEntry),
		cm:     claimDefaults(cfg.Claims),
	}
	return &OAuth2Authenticator{
		cfg:          cfg,
		kf:           kf,
		strategy:     "auto",
		introspector: intr,
	}
}

// Authenticate extracts and validates a Bearer token from r.
// Returns ErrNoCredentials when no Authorization header is present,
// ErrInvalidToken when the token is present but invalid, or another error
// when the key source is unavailable (callers should fail closed).
func (a *OAuth2Authenticator) Authenticate(r *http.Request) (*Principal, error) {
	raw, err := extractBearer(r)
	if err != nil {
		return nil, err // ErrNoCredentials
	}

	switch a.strategy {
	case "jwt":
		return a.authenticateJWT(raw)
	case "introspection":
		if a.introspector == nil {
			return nil, fmt.Errorf("auth: introspection strategy configured but no introspector available")
		}
		return a.introspector.introspect(r.Context(), raw)
	default: // "auto"
		if looksLikeJWT(raw) {
			return a.authenticateJWT(raw)
		}
		if a.introspector != nil {
			return a.introspector.introspect(r.Context(), raw)
		}
		// auto with no introspector → treat as JWT.
		return a.authenticateJWT(raw)
	}
}

// authenticateJWT validates raw as a signed JWT against the configured JWKS.
func (a *OAuth2Authenticator) authenticateJWT(raw string) (*Principal, error) {
	cfg := a.cfg
	cm := claimDefaults(cfg.Claims)

	token, err := jwt.Parse(
		raw,
		a.kf,
		jwt.WithValidMethods([]string{"RS256", "RS384", "RS512", "ES256", "ES384", "ES512"}),
		jwt.WithIssuer(cfg.Issuer),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidToken, err)
	}
	if !token.Valid {
		return nil, fmt.Errorf("%w: token not valid", ErrInvalidToken)
	}

	rawClaims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return nil, fmt.Errorf("%w: unexpected claims type", ErrInvalidToken)
	}

	// Audience check: require at least one of cfg.Audiences to be in aud.
	if len(cfg.Audiences) > 0 {
		if err := checkAudience(rawClaims, cfg.Audiences); err != nil {
			return nil, err
		}
	}

	p := buildPrincipal(rawClaims, cm)
	p.Authenticated = true
	return p, nil
}

// looksLikeJWT returns true if s has three dot-separated segments and the first
// segment base64url-decodes to a JSON object containing an "alg" key.
func looksLikeJWT(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return false
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return false
	}
	var header map[string]any
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return false
	}
	_, hasAlg := header["alg"]
	return hasAlg
}

// ---------------------------------------------------------------------------
// tokenIntrospector — RFC 7662
// ---------------------------------------------------------------------------

type tokenIntrospector struct {
	cfg    IntrospectionConfig
	client *http.Client
	cm     ClaimMap
	mu     sync.Mutex
	cache  map[[32]byte]introspectionCacheEntry
}

type introspectionCacheEntry struct {
	principal *Principal
	expiresAt time.Time
}

// introspect calls the introspection endpoint for the given raw token.
// It checks and updates the in-memory cache (keyed by sha256 of token).
func (ti *tokenIntrospector) introspect(ctx context.Context, token string) (*Principal, error) {
	key := sha256.Sum256([]byte(token))

	// Check cache.
	ti.mu.Lock()
	if entry, ok := ti.cache[key]; ok && time.Now().Before(entry.expiresAt) {
		p := entry.principal
		ti.mu.Unlock()
		return p, nil
	}
	ti.mu.Unlock()

	// Call introspection endpoint.
	form := url.Values{}
	form.Set("token", token)
	form.Set("token_type_hint", "access_token")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ti.cfg.Endpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("auth: introspection request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(ti.cfg.ClientID, ti.cfg.ClientSecret)

	resp, err := ti.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("auth: introspection request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("auth: introspection endpoint returned status %d", resp.StatusCode)
	}

	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("auth: decoding introspection response: %w", err)
	}

	// active must be boolean true.
	active, _ := body["active"].(bool)
	if !active {
		return nil, fmt.Errorf("%w: token is not active", ErrInvalidToken)
	}

	p := buildPrincipal(body, ti.cm)
	p.Authenticated = true

	// Compute cache expiry.
	ttl := ti.cfg.CacheTTL
	if ttl == 0 {
		ttl = 60 * time.Second
	}
	expiry := time.Now().Add(ttl)
	if expRaw, ok := body["exp"]; ok {
		switch expV := expRaw.(type) {
		case float64:
			tokenExp := time.Unix(int64(expV), 0)
			remaining := time.Until(tokenExp)
			if remaining <= 0 {
				remaining = 0
			}
			if remaining < ttl {
				expiry = time.Now().Add(remaining)
			}
		}
	}

	ti.mu.Lock()
	ti.cache[key] = introspectionCacheEntry{principal: p, expiresAt: expiry}
	ti.mu.Unlock()

	return p, nil
}

// ---------------------------------------------------------------------------
// HeaderAuthenticator
// ---------------------------------------------------------------------------

// HeaderConfig holds the names of the trusted identity headers.
type HeaderConfig struct {
	ConsumerHeader string
	UserTypeHeader string
	GroupsHeader   string // comma/space-separated values
	RolesHeader    string
	ScopesHeader   string
}

// HeaderAuthenticator reads identity from trusted proxy-injected headers.
type HeaderAuthenticator struct {
	cfg HeaderConfig
}

// NewHeaderAuthenticator returns a HeaderAuthenticator configured with cfg.
func NewHeaderAuthenticator(cfg HeaderConfig) *HeaderAuthenticator {
	return &HeaderAuthenticator{cfg: cfg}
}

// Authenticate reads the configured headers from r.
// When the consumer header is absent the returned Principal has Authenticated=false
// and no error is returned (header mode never hard-fails).
func (h *HeaderAuthenticator) Authenticate(r *http.Request) (*Principal, error) {
	consumer := ""
	if h.cfg.ConsumerHeader != "" {
		consumer = r.Header.Get(h.cfg.ConsumerHeader)
	}

	p := &Principal{
		Subject:       consumer,
		Consumer:      consumer,
		Authenticated: consumer != "",
	}

	if h.cfg.UserTypeHeader != "" {
		p.UserType = r.Header.Get(h.cfg.UserTypeHeader)
	}
	if h.cfg.GroupsHeader != "" {
		p.Groups = splitTokens(r.Header.Get(h.cfg.GroupsHeader))
	}
	if h.cfg.RolesHeader != "" {
		p.Roles = splitTokens(r.Header.Get(h.cfg.RolesHeader))
	}
	if h.cfg.ScopesHeader != "" {
		p.Scopes = splitTokens(r.Header.Get(h.cfg.ScopesHeader))
	}

	return p, nil
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// extractBearer pulls the bearer token from the Authorization header.
func extractBearer(r *http.Request) (string, error) {
	hdr := r.Header.Get("Authorization")
	if hdr == "" {
		return "", ErrNoCredentials
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(hdr, prefix) {
		return "", ErrNoCredentials
	}
	raw := strings.TrimSpace(hdr[len(prefix):])
	if raw == "" {
		return "", ErrNoCredentials
	}
	return raw, nil
}

// checkAudience verifies that at least one of the required audiences appears
// in the token's aud claim. The aud claim may be a string or an array.
func checkAudience(claims jwt.MapClaims, required []string) error {
	audClaim, exists := claims["aud"]
	if !exists {
		return fmt.Errorf("%w: missing aud claim", ErrInvalidToken)
	}

	var tokenAuds []string
	switch v := audClaim.(type) {
	case string:
		tokenAuds = []string{v}
	case []any:
		for _, a := range v {
			if s, ok := a.(string); ok {
				tokenAuds = append(tokenAuds, s)
			}
		}
	case []string:
		tokenAuds = v
	default:
		return fmt.Errorf("%w: unrecognised aud claim type %T", ErrInvalidToken, audClaim)
	}

	for _, req := range required {
		for _, tok := range tokenAuds {
			if req == tok {
				return nil
			}
		}
	}
	return fmt.Errorf("%w: audience mismatch: token %v not in required %v", ErrInvalidToken, tokenAuds, required)
}

// buildPrincipal extracts fields from rawClaims according to cm.
func buildPrincipal(claims map[string]any, cm ClaimMap) *Principal {
	p := &Principal{}
	p.Subject = claimString(claims, cm.Subject)
	if cm.Consumer != "" {
		p.Consumer = claimString(claims, cm.Consumer)
	}
	p.Scopes = claimStrings(claims, cm.Scopes)
	if cm.Groups != "" {
		p.Groups = claimStrings(claims, cm.Groups)
	}
	if cm.Roles != "" {
		p.Roles = claimStrings(claims, cm.Roles)
	}
	return p
}

// claimGet walks a dotted path "a.b.c" through nested map[string]any values.
// Returns nil if any segment is missing or the intermediate value is not a map.
func claimGet(claims map[string]any, path string) any {
	parts := strings.SplitN(path, ".", 2)
	val, ok := claims[parts[0]]
	if !ok {
		return nil
	}
	if len(parts) == 1 {
		return val
	}
	nested, ok := val.(map[string]any)
	if !ok {
		return nil
	}
	return claimGet(nested, parts[1])
}

// claimString returns a single string value for a claim path.
func claimString(claims map[string]any, path string) string {
	v := claimGet(claims, path)
	if v == nil {
		return ""
	}
	s, _ := v.(string)
	return s
}

// claimStrings returns a []string from a claim that may be:
//   - a space-delimited string (OAuth2 "scope" convention)
//   - a []string
//   - a []any of strings
func claimStrings(claims map[string]any, path string) []string {
	v := claimGet(claims, path)
	if v == nil {
		return nil
	}
	switch val := v.(type) {
	case string:
		return splitTokens(val)
	case []string:
		return val
	case []any:
		var out []string
		for _, a := range val {
			if s, ok := a.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// splitTokens splits a string on commas and whitespace, dropping empty fields.
func splitTokens(s string) []string {
	if s == "" {
		return nil
	}
	// Replace commas with spaces then split on fields.
	s = strings.ReplaceAll(s, ",", " ")
	parts := strings.Fields(s)
	if len(parts) == 0 {
		return nil
	}
	return parts
}

// oidcMeta holds the fields we care about from an OpenID Connect metadata document.
type oidcMeta struct {
	JWKSURI               string `json:"jwks_uri"`
	IntrospectionEndpoint string `json:"introspection_endpoint"`
}

// discoverOIDCEndpoints fetches the OpenID Connect metadata for issuer and returns
// the jwks_uri and introspection_endpoint fields.
func discoverOIDCEndpoints(issuer string) (jwksURI, introspectionEndpoint string, err error) {
	wellKnown := strings.TrimRight(issuer, "/") + "/.well-known/openid-configuration"
	resp, err := http.Get(wellKnown) //nolint:noctx // discovery is called only at startup
	if err != nil {
		return "", "", fmt.Errorf("GET %s: %w", wellKnown, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("GET %s: unexpected status %d", wellKnown, resp.StatusCode)
	}

	var meta oidcMeta
	if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
		return "", "", fmt.Errorf("decode OpenID metadata: %w", err)
	}
	if meta.JWKSURI == "" {
		return "", "", fmt.Errorf("openid-configuration at %s has no jwks_uri", wellKnown)
	}
	return meta.JWKSURI, meta.IntrospectionEndpoint, nil
}

// discoverJWKSURL fetches the OpenID Connect metadata for issuer and returns
// the jwks_uri field. Kept for backward compatibility.
func discoverJWKSURL(issuer string) (string, error) {
	jwksURI, _, err := discoverOIDCEndpoints(issuer)
	return jwksURI, err
}
