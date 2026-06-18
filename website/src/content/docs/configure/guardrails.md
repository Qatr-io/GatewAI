---
title: Guardrails
---

# Guardrails

GatewAI can scan LLM requests for **PII** and **secrets** before they reach the backend. Guardrails run on the sync LLM-proxy path (`POST /v1/*`) and are configured per service.

## Configuration

Guardrails are enabled on a service by declaring a non-empty `checks` list:

```yaml
services:
  - type: llm
    model: "chat-smart"
    provider: passthrough
    guardrails:
      checks: [pii, secrets]   # which detector groups to run
      action: block            # block (default) | redact | flag
```

Guardrails are **opt-in per service** — omit the `guardrails` block (or leave `checks` empty) to disable them. There is no global toggle.

## Actions

| Action | Behaviour |
|---|---|
| `block` (default) | Reject the request with `422 Unprocessable Entity`; the backend is never called. |
| `redact` | Replace each match in-place with a placeholder (e.g. `[REDACTED_EMAIL]`) and forward the cleaned body. |
| `flag` | Forward the request unchanged; only emit a log line and a metric. |

## Check groups

`checks` selects which detector groups run. Country groups are **strictly opt-in**.

| Group | Detects |
|---|---|
| `pii` | email, credit card, IBAN, IPv4 address, international (E.164) phone |
| `pii_fr` | French phone, NIR (social security), SIREN, SIRET |
| `pii_us` | US Social Security Number |
| `pii_uk` | UK National Insurance Number |
| `pii_es` | Spanish DNI / NIF |
| `pii_it` | Italian Codice Fiscale |
| `secrets` | AWS access keys, private-key blocks, JWTs, GitHub / Slack / Google tokens |

## How it works

The scanner reads the `messages` array of the OpenAI-compatible JSON body and inspects all text content:

- `messages[*].content` as a string (standard chat payload)
- `messages[*].content[*].text` as content parts (multimodal payloads)

For `redact`, matches are replaced inside that content and the body is re-serialised as valid JSON; all other fields are preserved. Placeholders are not re-matched, so redaction is idempotent.

## HTTP response (block)

```
HTTP/1.1 422 Unprocessable Entity
Content-Type: application/json

{"error": "guardrails violation: email, aws_access_key"}
```

## Metrics

```
gatewai_guardrails_total{service_type, model, action, result}   # result = blocked | redacted | flagged
gatewai_guardrails_pii_blocked_total{service_type, model}        # retained for continuity (block only)
```

## False positives

:::caution
Purely numeric national-ID patterns — `siren` (9 digits), `siret` (14 digits), `fr_nir`, `us_ssn`, `es_dni` — have materially higher false-positive rates than the others. Enable the relevant country group per service only after reviewing a representative sample of your payloads, and consider `flag` before `block` or `redact`.
:::
