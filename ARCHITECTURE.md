# GatewAI — Architecture

> ⚠️ **Ce document est en cours de mise à jour.** Les diagrammes ci-dessous reflètent l'ancienne architecture Kafka.
> L'architecture actuelle utilise Redis (listes `relay:<model>:pending` via BLMOVE/RPUSH) à la place de Kafka.
> Voir `CLAUDE.md` pour la description à jour du flux de données.

## Vue d'ensemble

```
┌──────────────────────────────────────────────────────────────────────────────────┐
│                              KEVENT — Vue d'ensemble                              │
└──────────────────────────────────────────────────────────────────────────────────┘

  CLIENT
    │
    │  ① Mode ASYNC                    ② Mode SYNC (OpenAI-compatible)
    │  POST /jobs                       POST /v1/audio/transcriptions
    │  multipart: type + file           POST /v1/chat/completions
    │  GET  /jobs/{id}                  (payload OpenAI standard, field "model")
    │  Webhook (callback_url)
    ▼
┌────────────────────────────────────────────────────────────────────────────────┐
│  GATEWAY  (Deployment Kubernetes, chi router)                                  │
│                                                                                │
│   ┌─────────────────────────────────────────────────────────────────────────┐ │
│   │  JobHandler  (mode async)                  POST /jobs • GET /jobs/{id}  │ │
│   │    Submit :  S3.Upload → Redis.SaveJob → Kafka.Publish                  │ │
│   │    GetStatus: Redis.GetJob → S3.PresignedGetURL (si completed)          │ │
│   └─────────────────────────────────────────────────────────────────────────┘ │
│   ┌─────────────────────────────────────────────────────────────────────────┐ │
│   │  SyncHandler  (mode sync)                          POST /v1/*           │ │
│   │    ① Extrait "model" du payload (multipart ou JSON)                     │ │
│   │    ② registry.RouteSync(path, model) → InferenceURL                    │ │
│   │    ③ Proxy HTTP → Dispatcher Knative /v1/*                              │ │
│   │    ④ Stream réponse directement au client                               │ │
│   └─────────────────────────────────────────────────────────────────────────┘ │
│   ┌─────────────────────────────────────────────────────────────────────────┐ │
│   │  ConsumerManager  (1 goroutine / result topic)    mode async seulement  │ │
│   │    Kafka.FetchMessage ← jobs.{type}.results  (ResultEvent)              │ │
│   │    ① Redis.UpdateJobResult → status: completed|failed                   │ │
│   │    ② sendWebhook(callback_url)  ×3 avec backoff exponentiel            │ │
│   └─────────────────────────────────────────────────────────────────────────┘ │
└────────────────────────────────────────────────────────────────────────────────┘
    │  S3 / Redis / Kafka                    │  HTTP proxy
    ▼  (mode async)                          ▼  (mode sync)
┌──────────────────┐                ┌────────────────────────────────────────────┐
│  Scaleway S3     │                │  Knative Services (1 par type de service)  │
│                  │                │                                            │
│  {job_id}/       │                │  GatewAI-transcription-predictor            │
│  input.ext  ◀────┼────────────────│  ┌──────────────────────┐ ┌────────────┐  │
│  result.json────▶│                │  │  dispatcher  :8080   │ │ whisper    │  │
└──────────────────┘                │  │  ─────────────────── │ │ :9000      │  │
                                    │  │  POST /   async       │ │            │  │
┌──────────────────┐                │  │   KafkaSource↗  ─────┼▶│  GPU       │  │
│  Kafka (SASL_SSL)│                │  │  POST /v1/* sync      │ │  inférence │  │
│  port 9093       │                │  │   Gateway↗      ─────┼▶│            │  │
│                  │                │  └──────────────────────┘ └────────────┘  │
│  jobs.*.input ───┼── KafkaSource─▶│                                            │
│  jobs.*.results◀─┼────────────────│  GatewAI-dispatcher-ocr  (même structure)  │
└──────────────────┘                └────────────────────────────────────────────┘
```

---

## Flux 1 — Soumission d'un job (mode async)

```
Client          Gateway          Redis         S3 (Scaleway)     Kafka
  │                │               │                │               │
  │─POST /jobs────▶│               │                │               │
  │  type=transcr  │               │                │               │
  │  file=audio.wav│               │                │               │
  │                │─ValidateFile  │                │               │
  │                │  (ext, size)  │                │               │
  │                │──PutObject────────────────────▶│               │
  │                │  abc123/input.wav              │               │
  │                │◀──────────────────────────────│               │
  │                │──SaveJob─────▶│               │               │
  │                │  {id, status: │               │               │
  │                │   pending}    │               │               │
  │                │◀──────────────│               │               │
  │                │──PublishInputEvent────────────────────────────▶│
  │                │  { job_id, input_ref }         │  jobs.transcr │
  │◀─202───────────│               │                │  .input       │
  │  { job_id,     │               │                │               │
  │    status:     │               │                │               │
  │    "pending" } │               │                │               │
```

---

## Flux 2 — Traitement asynchrone (dispatcher sidecar + GPU)

```
Kafka         KafkaSource       Dispatcher :8080   Whisper :9000    S3          Kafka
  │               │                  │                   │            │            │
  │  jobs.transcr │                  │                   │            │            │
  │  .input       │                  │                   │            │            │
  │──FetchMessage▶│                  │                   │            │            │
  │               │                  │                   │            │            │
  │               │  [cold start : readinessProbe attend que whisper soit ready]  │
  │               │                  │                   │            │            │
  │               │─POST /───────────▶                  │            │            │
  │               │  CloudEvent       │                   │            │            │
  │               │  { job_id,        │                   │            │            │
  │               │    input_ref }    │                   │            │            │
  │               │                  │                   │            │            │
  │               │              [handler bloque — Knative KPA voit 1 req en vol] │
  │               │                  │                   │            │            │
  │               │                  │──GetObject────────────────────▶│            │
  │               │                  │  abc123/input.wav │            │            │
  │               │                  │◀──────────────────────────────│            │
  │               │                  │  io.ReadCloser (stream)        │            │
  │               │                  │──POST /v1/audio/─▶│            │            │
  │               │                  │  transcriptions   │            │            │
  │               │                  │  (multipart pipe) │─GPU────────│            │
  │               │                  │                   │  inférence │            │
  │               │                  │◀──────────────────│  ~2-5 min  │            │
  │               │                  │  { text: "..." }  │            │            │
  │               │                  │──PutObject────────────────────▶│            │
  │               │                  │  abc123/result.json            │            │
  │               │                  │──PublishResultEvent────────────────────────▶│
  │               │                  │  { completed, result_ref }     │  jobs.    │
  │               │◀─200─────────────│                   │            │  .results │
  │◀─CommitOffset─│                  │                   │            │            │
```

---

## Flux 3 — Retour du résultat (mode async)

```
Kafka          Gateway                Redis           Client
  │         ConsumerManager             │               │
  │               │                     │               │
  │  jobs.transcr │                     │               │
  │  .results     │                     │               │
  │──FetchMessage▶│                     │               │
  │               │──UpdateJobResult────▶               │
  │               │  { status: completed,│               │
  │               │    result_ref }      │               │
  │◀─CommitOffset─│◀────────────────────│               │
  │               │──sendWebhook (goroutine)────────────▶│
  │               │  POST callback_url  │  retry ×3    │
  │               │  { job_id, status,  │  backoff ×2  │
  │               │    result_ref }     │               │
  │               │                     │               │
  │               │                     │               │◀─GET /jobs/abc123
  │               │◀────────────────────│──GetJob───────│
  │               │  PresignedGetURL    │               │
  │               │  (S3, TTL 60 min)   │               │
  │               │─────────────────────────────────────▶│
  │               │  { status: "completed",              │
  │               │    result_url: "https://s3.fr-par... │
  │               │    ?X-Amz-Expires=3600" }            │
```

---

## Flux 4 — Requête synchrone (proxy OpenAI-compatible)

```
Client (SDK OpenAI)     Gateway SyncHandler     Dispatcher /v1/*     Whisper :9000
       │                        │                       │                    │
       │─POST /v1/audio/────────▶                      │                    │
       │  transcriptions        │                       │                    │
       │  model=whisper-large-v3│                       │                    │
       │  file=audio.wav        │                       │                    │
       │                        │                       │                    │
       │                        │  ParseMultipartForm   │                    │
       │                        │  r.FormValue("model") │                    │
       │                        │  RouteSync(path,model)│                    │
       │                        │  → InferenceURL       │                    │
       │                        │                       │                    │
       │                        │─POST /v1/audio/───────▶                   │
       │                        │  transcriptions        │                    │
       │                        │  (body reconstruit     │                    │
       │                        │   via io.Pipe)         │                    │
       │                        │                        │─POST /v1/audio/───▶
       │                        │                        │  transcriptions    │
       │                        │                        │  (transparent fwd) │
       │                        │                        │                    │─GPU─▶
       │                        │                        │                    │ inférence
       │                        │                        │◀───────────────────│
       │                        │◀───────────────────────│  { text: "..." }   │
       │◀────────────────────────│  stream réponse        │                    │
       │  { text: "..." }        │                        │                    │
```

> Aucun passage par S3, Redis ou Kafka : la réponse est streamée bout-en-bout.

---

## Scaling Knative — comportement GPU

```
                        jobs.transcription.input  (lag Kafka)

  msgs  5 ──│                    ████
  en    4 ──│                ████████
  queue 3 ──│            ████████████
        2 ──│        ████████████████
        1 ──│    ████████████████████
        0 ──│────────────────────────────────────────────────▶ temps
             silence   burst          traitement   silence

  pods  0 ──│    ┌───┐
  actifs 1 ──│    │   └───┐
        2 ──│    │       └───┐
        3 ──│    │           └───┐
        4 ──│    │               └───┐
        5 ──│    │                   └───────────────────────▶
             ▲   ▲                   ▲           ▲
          scale  cold start        traitement  scale
          to 0   (~30s model load)  en cours   to 0
                                               (scaleToZeroGracePeriod)

  GPU   $$  0   [$][$][$][$][$]    [$][$][$]  0
  coût  ────────────────────────────────────────────▶ temps

  containerConcurrency: 1  →  1 message en vol = 1 pod = 1 GPU
  maxScale: N              →  jamais plus de N pods/GPUs simultanés
```

> En mode sync, le KPA Knative mesure les requêtes HTTP en cours sur le dispatcher.
> Une requête de transcription = 1 pod GPU occupé pendant toute la durée de l'inférence.

---

## Structure des données

### Topics Kafka (mode async)

| Topic | Produit par | Consommé par | Clé message |
|---|---|---|---|
| `jobs.transcription.input` | Gateway | KafkaSource → dispatcher | `job_id` |
| `jobs.diarization.input` | Gateway | KafkaSource → dispatcher | `job_id` |
| `jobs.ocr.input` | Gateway | KafkaSource → dispatcher | `job_id` |
| `jobs.transcription.results` | Dispatcher | Gateway ConsumerManager | `job_id` |
| `jobs.diarization.results` | Dispatcher | Gateway ConsumerManager | `job_id` |
| `jobs.ocr.results` | Dispatcher | Gateway ConsumerManager | `job_id` |

### Objets S3 (mode async)

```
GatewAI-jobs/
└── {job_id}/
    ├── input.wav          ← uploadé par le gateway au POST /jobs
    └── result.json        ← uploadé par le dispatcher après inférence
```

### État Redis (TTL 72h, mode async uniquement)

```json
{
  "id":           "abc123",
  "service_type": "transcription",
  "status":       "pending | processing | completed | failed",
  "input_ref":    "abc123/input.wav",
  "result_ref":   "abc123/result.json",
  "callback_url": "https://client.example.com/webhook",
  "error":        "",
  "created_at":   "2026-03-05T10:00:00Z",
  "updated_at":   "2026-03-05T10:05:32Z"
}
```

### Routing sync (registre en mémoire)

```
openai_path                      model                    InferenceURL
/v1/audio/transcriptions   →   whisper-large-v3   →   http://GatewAI-transcription-predictor…
/v1/chat/completions        →   llava-v1.6-…       →   http://GatewAI-dispatcher-ocr…
```

Plusieurs services peuvent partager le même `openai_path` (ex: deux modèles sur `/v1/chat/completions`) — la sélection se fait uniquement par la valeur du champ `model`.

---

## Sémantique des codes HTTP du dispatcher (mode async)

| Code | Signification | Comportement relay |
|---|---|---|
| `200` | Job traité (succès ou échec métier publié via ResultEvent) | Offset commité, pas de retry |
| `400` | Message malformé (JSON invalide, job_id manquant) | Pas de retry |
| `500` | Erreur infrastructure transitoire (S3 indisponible, réseau) | Retry selon delivery config |

---


---

## Déploiement Kubernetes

> **TODO** — Cette section est à réécrire pour refléter l'architecture Redis-based actuelle.
> Voir `k8s/deployment-transcription.yaml` et `k8s/inference-transcription.yaml` pour les manifests à jour.
