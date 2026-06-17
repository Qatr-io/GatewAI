# PII guardrails

GatewAI includes a PII detection layer for LLM requests. When enabled on a service, incoming `POST /v1/*` JSON requests are scanned before being forwarded to the backend. Requests containing PII are rejected with `400 Bad Request`.

## Configuration

Enable per service in `config.yaml`:

```yaml
services:
  - type: llm
    model: "chat-smart"
    provider: passthrough
    guardrails:
      pii: true
```

`guardrails.pii` is `false` by default. There is no global toggle — enable selectively per model after assessing your payload characteristics.

## How it works

The scanner deserialises the `messages` array of the OpenAI-compatible JSON body and extracts all text content:

- `messages[*].content` as a string (standard chat payload)
- `messages[*].content[*].text` as content parts (multimodal payloads)

Each text is matched against six compiled regular expressions. The first match per category stops scanning for that category.

## Patterns detected

| Pattern | What it matches | Notes |
|---|---|---|
| `email` | Standard email addresses | Low false-positive rate |
| `phone_fr` | French phone numbers (`+33`, `0033`, or `0x` format, groups of 2 digits) | |
| `iban` | IBAN (`CC99XXXX…`) | |
| `credit_card` | 13–16 digit sequences, optionally space- or dash-separated | Can match long numeric strings |
| `siret` | 14-digit SIRET | Checked before SIREN to avoid double-reporting |
| `siren` | 9-digit SIREN | Higher false-positive rate — any 9-digit number matches |

## HTTP response

On detection, the gateway returns:

```
HTTP/1.1 400 Bad Request
Content-Type: application/json

{"error": "pii detected", "violations": ["email", "phone_fr"]}
```

The `violations` array lists every category detected in the request.

## Metrics

```
GatewAI_guardrails_pii_blocked_total{service_type, model}
```

Incremented once per blocked request. Labels match the service entry.

## False positives

!!! warning
    The `siren` pattern (`\b\d{9}\b`) matches any 9-digit number. The `siret` pattern (`\b\d{14}\b`) similarly matches any 14-digit sequence. Both have materially higher false-positive rates than the other patterns.

    Enable `guardrails.pii: true` only after reviewing a representative sample of your payload content.
