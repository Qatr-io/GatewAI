---
title: Authentication
---

# Authentication

GatewAI can authenticate requests itself, or trust identity supplied by an upstream reverse proxy. Authentication is **optional** and chosen per deployment via a top-level `auth` block. When `auth` is absent, the gateway performs no authentication — identity is read from the upstream headers (the default when the gateway sits behind a reverse proxy that authenticates).

Pick **one mode per deployment**.

## OAuth2 mode

The gateway acts as an OAuth2 **resource server**: it validates the **access token** presented as `Authorization: Bearer <token>`.

```yaml
auth:
  mode: oauth2
  oauth2:
    issuer: "https://idp.example.com/realm"   # discovery → jwks_uri
    jwks_url: ""                               # optional explicit override
    audiences: ["gatewai"]                     # at least one must match the token aud
    claims:                                    # dotted paths; configure per IdP
      consumer: "preferred_username"
      scopes:   "scope"
      groups:   "groups"
      roles:    "roles"
```

What it does:

- Verifies the JWT **signature** against the IdP's JWKS (fetched via issuer discovery, cached and refreshed), and checks `iss`, `aud`, and `exp`. Only asymmetric algorithms (RS*/ES*) are accepted.
- Maps claims into a principal: `subject`, `consumer`, `scopes`, `groups`, `roles`. `scope` may be the OAuth2 space-delimited string or an array; `groups`/`roles` may be arrays or single values; claim names support dotted paths for nested claims.
- **Fails closed**: a missing or invalid token returns `401`; if the IdP/JWKS is unreachable it returns `503` (never silently allows traffic).
- **Strips the client bearer** before proxying to inference backends — the token's audience is the gateway, not the backend. Backends authenticate with their own `inference_headers`.

### Token format: JWT or opaque

`validation` selects how the access token is checked:

```yaml
auth:
  oauth2:
    validation: auto            # auto | jwt | introspection
    introspection:              # required for opaque tokens (RFC 7662)
      client_id: "${OAUTH_CLIENT_ID}"
      client_secret: "${OAUTH_CLIENT_SECRET}"
      cache_ttl: 60s
```

- **`jwt`** — verify the token locally via JWKS (no per-request IdP call).
- **`introspection`** — POST the token to the IdP's introspection endpoint with the gateway's client credentials; the IdP returns `active` plus claims. Results are cached (up to `cache_ttl`, capped by the token's `exp`). This validates **opaque** tokens and gives **live revocation** — a revoked token stops working immediately, instead of remaining valid until expiry.
- **`auto`** (default) — JWT-shaped tokens take the JWT path; everything else is introspected (requires an `introspection` block).

Introspection fails closed: if the endpoint is unreachable or returns a non-2xx, the request is rejected.

### Metrics

```
gatewai_auth_oauth2_duration_seconds{operation}          # operation = jwt | introspection
gatewai_auth_oauth2_errors_total{operation, reason}       # reason = invalid_token | unreachable
```

## Proxy mode

The gateway trusts identity headers set by an upstream reverse proxy that has already authenticated the caller.

```yaml
auth:
  mode: proxy
  proxy:
    consumer_header: "X-Consumer-Username"
    user_type_header: "X-Consumer-Type"
    groups_header:   "X-Consumer-Groups"
    roles_header:    "X-Consumer-Roles"
```

Use this only when those headers cannot be set by untrusted clients (i.e. the gateway is not directly reachable). Group/role headers are comma- or space-separated.

## Exempt routes

`/health`, `/metrics`, `/docs`, and `/openapi.yaml` never require authentication, so probes and scrapers keep working.

## Notes

- Auth configuration changes require a gateway **restart** (they are not hot-reloaded).
- Identity resolved in OAuth2 mode is used internally for rate limiting and job ownership; it is not forwarded to inference backends.
