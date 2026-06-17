# GatewAI

API Gateway pour les services d'inférence KServe sur Kubernetes — file async Redis, proxy sync compatible OpenAI, et proxy LLM dans un seul binaire.

| Mode | Endpoint | Quand l'utiliser |
|---|---|---|
| **Async** | `POST /jobs/{service_type}` | Fichiers lourds, traitements longs (>30s), webhook |
| **Sync** | `POST /v1/*` | SDK OpenAI, services faible latence |
| **LLM proxy** | `POST /v1/*` (JSON + `provider`) | OpenAI / Anthropic / Ollama / vLLM avec cache et métriques |

## Architecture

![Architecture overview](docs/architecture/overview.png)

## Fonctionnalités

- Registre de services piloté par la config — ajouter un modèle avec un bloc YAML, sans modifier le code
- Hot-reload via `POST /-/reload`
- Routage prioritaire (Redis `LPUSH`)
- Suivi des consommateurs via un header configurable
- Proxy LLM — OpenAI / Anthropic / Ollama / passthrough (vLLM), traduction de provider
- Cache des réponses (Redis, clé SHA-256, header `X-Cache`)
- Routage multi-backend — blue/green, canary, fallback avec sélection pondérée
- Rate limiting par consommateur — fenêtre fixe, par type de service et type d'utilisateur, limites de requêtes et de tokens
- Guardrails PII — bloque les requêtes LLM contenant email, téléphone, IBAN, carte bancaire, SIREN/SIRET
- Audit trail — log slog structuré par requête LLM (opt-in)
- Chiffrement AES-256-GCM at-rest pour les objets S3
- Autoscaling KEDA — le relay scale sur la profondeur de la file Redis, scale-to-zero
- Métriques Prometheus — requêtes, latence, tokens, cache, rate limits, top-N consommateurs

## Documentation

Référence de configuration, guide de déploiement, référence API et runbooks :
**[qatr-io.github.io/GatewAI](https://qatr-io.github.io/GatewAI)**
