---
title: Access control
---

# Access control

GatewAI can restrict which models a caller may use, based on their identity (groups, roles, scopes, …). Access control is **optional** and **default-deny**: when a `policies` block is configured, a request is allowed only if a rule both matches the caller and grants the requested model. With no `policies` block there is no access control.

Access control **requires authentication** (`auth.mode` set) — it evaluates the `Principal` resolved by the [authentication](/configure/authentication/) layer, so the gateway refuses to start with `policies` but no `auth.mode`.

## Configuration

```yaml
policies:
  default: deny                 # deny (allowlist) | allow (enforcement disabled)
  rules:
    - match:
        groups: ["research-lab"]      # match fields: groups, roles, scopes, consumers, user_types
      allow_models: ["gpt-oss-120b", "chat-*"]   # globs against the model alias
      allow_service_types: ["llm"]               # optional; globs against service type
    - match: { roles: ["admin"] }
      allow_models: ["*"]
    - match: { scopes: ["chat.write"] }
      allow_models: ["chat-small"]
```

## How a request is decided

1. The request is routed, resolving its **service type** and **model**.
2. Each rule is evaluated:
   - **Identity match** — for every *non-empty* field in `match`, the caller must have at least one of the listed values (group/role/scope membership, or an exact consumer/user-type). All specified fields must match (AND). An empty `match: {}` matches everyone.
   - **Resource grant** — the requested model must match one of `allow_models` (glob), and if `allow_service_types` is set, the service type must match one of those globs. An empty `allow_models` grants nothing (list `"*"` to allow all).
3. If any rule both matches and grants → **allowed**. Otherwise → **`403 Forbidden`**.

Set `default: allow` to disable enforcement entirely (e.g. while staging policies).

## Where it applies

Enforced on both the sync OpenAI-compatible path (`POST /v1/*`) and the async job path (`POST /jobs/{type}`), immediately after routing resolves the model — before any backend work.

## Observability

- Denials are logged as security events (with the caller's consumer and the model).
- Metric: `gatewai_authz_decisions_total{service_type, model, decision}` (`decision` = `allow` | `deny`).

Policies are **hot-reloadable** via the config reload endpoint.
