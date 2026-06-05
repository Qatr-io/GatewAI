# gatewai-gateway

API Gateway pour les services d'inférence KServe. Plusieurs modes de fonctionnement coexistent pour chaque service :

| Mode | Endpoints | Quand l'utiliser |
|---|---|---|
| **Async** (Redis queue) | `POST /jobs/{service_type}`, `GET /jobs/{service_type}/{id}`, `GET /jobs` | Fichiers lourds, traitements longs (>30s), besoin de webhook |
| **Sync direct proxy** | `POST /v1/*` (JSON ou multipart) | Intégration SDK OpenAI, services sync-only (reranker, embeddings…) |
| **LLM proxy** | `POST /v1/*` JSON + `provider` configuré | Proxying LLM (OpenAI, Anthropic, Ollama, vLLM) avec cache et métriques |

## Architecture

### Mode async

```
Client
  │
  ▼
POST /jobs/{service_type} (multipart: file, model, operation?)
  │
  ├─ 1. Fichier → S3
  ├─ 2. Job record → Redis (status: pending)
  └─ 3. Job ID → Redis list relay:<model>:pending  (RPUSH)
                          │
                          ▼
                    Relay Deployment (un job par cycle de vie du pod)
                                              │
                                              ├─ BLMOVE relay:<model>:pending → relay:<model>:processing
                                              ├─ Download fichier S3
                                              ├─ POST multipart → modèle GPU (127.0.0.1:9000/<inference_url>)
                                              ├─ Upload result.json → S3
                                              └─ PUBLISH jobs:<model>:completed  (Redis pub/sub)
                                                                    │
                                              ┌─────────────────────┘
                                              │  (consumer.Manager interne gateway)
                                              ▼
                                       Redis mis à jour (status: completed/failed)
                                       + Webhook POST si callback_url fourni
Client
  │
  ▼
GET /jobs/{service_type}/{id}  →  { status, result (inline JSON) }
```

### Mode sync (proxy OpenAI-compatible)

**Direct proxy** (requête `application/json` ou `multipart/form-data`) :
```
Client  POST /v1/*
  │
  ▼
Gateway → HTTP proxy → inference_url + chemin d'origine → modèle GPU
  │
  ▼ (réponse streamée directement)
Client
```

**LLM proxy** (requête `application/json` + service avec `provider` configuré) :
```
Client  POST /v1/chat/completions  {"model": "my-alias", ...}
  │
  ▼
Gateway — LLM proxy
  ├── Vérification cache Redis (clé SHA-256 du body canonique)  ── HIT → réponse + X-Cache: HIT
  │                                                                            ↑
  ├── MISS → pour chaque backend (ordre weighted-random) :         (async goroutine, 5s)
  │     ├── Réécriture model alias → backend.model (ou backend_model)
  │     ├── Injection backend.headers (override inference_headers)
  │     ├── Traduction requête (si anthropic : OpenAI → Messages API)
  │     ├── Forwarding vers backend URL
  │     └── Erreur réseau / 5xx → backend suivant ; 4xx → stop
  ├── Traduction réponse → format OpenAI
  ├── Métriques tokens + tracking consumer (Redis sorted set)
  └── Réponse client  X-Cache: MISS  +  cache-fill async

  Streaming (`"stream": true`) : SSE pipé directement, pas de cache ni de traduction.
  Retry possible avant WriteHeader ; impossible une fois le stream démarré.
```

### Composants externes requis

| Composant | Rôle | Requis |
|---|---|---|
| **Redis** | État des jobs, queue relay, pub/sub completion, cache LLM, rate limiting | Toujours |
| **S3** | Stockage fichiers d'entrée et résultats | Toujours |

---

## Démarrage rapide

### Prérequis

- Go 1.23+
- Redis et un bucket S3-compatible accessibles

### Build

```bash
# Gateway
go build -ldflags "-X main.version=v0.4.11" -o gateway ./cmd/gateway
CONFIG_PATH=/etc/GatewAI/config.yaml ./gateway

# Relay
cd relay
go build -o relay ./cmd/relay
CONFIG_PATH=/etc/relay/config.yaml ./relay
```

### Docker

```bash
docker build -t gatewai-gateway .
docker run \
  -e S3_ACCESS_KEY=... \
  -e S3_SECRET_KEY=... \
  -e REDIS_ADDR=redis:6379 \
  -p 8080:8080 \
  gatewai-gateway
```

---

## Configuration

La configuration est lue depuis `config.yaml` (chemin par défaut). Toutes les valeurs de la forme `${VAR:-défaut}` sont substituées depuis l'environnement au démarrage.

### Gateway (`config.yaml`)

```yaml
server:
  addr: ":8080"
  read_timeout: 120s    # élevé pour les gros uploads
  write_timeout: 0s     # 0 = désactivé — requis pour le mode sync (inférence longue)
  idle_timeout: 120s
  # consumer_header: header HTTP injecté par APISIX après auth (ex: "X-Consumer-Username").
  # Active le tracking consumer : GET /jobs, isolation des jobs, métrique par consumer.
  # Laisser vide en l'absence d'auth en amont.
  consumer_header: "${CONSUMER_HEADER:-}"
  # priority_header: header HTTP pour le routage prioritaire (ex: "X-Priority").
  # Si présent, peut être utilisé pour du routage prioritaire applicatif.
  priority_header: "${PRIORITY_HEADER:-}"
  # user_type_header: header HTTP pour le type d'utilisateur (ex: "X-User-Type" → "sa" | "user").
  # Utilisé pour le rate limiting et le labelling des métriques LLM.
  user_type_header: "${USER_TYPE_HEADER:-}"

s3:
  endpoint: "https://s3.fr-par.scw.cloud"
  region: "fr-par"
  access_key: "${S3_ACCESS_KEY}"
  secret_key: "${S3_SECRET_KEY}"
  bucket: "GatewAI-jobs"

encryption:
  key: "${ENCRYPTION_KEY:-}"    # AES-256-GCM at-rest, vide = désactivé

redis:
  addr: "redis:6379"
  password: ""
  db: 0
  job_ttl_hours: 72

# Métriques haute cardinalité — consumer token tracking
metrics:
  top_consumers: 10      # expose le top-N dans Prometheus via Redis sorted sets; 0 = désactivé

# Rate limiting par consumer, service type et user type (Redis fixed-window)
rate_limits:
  audio:
    unlimited:           # rate: 0 = aucune limite (Redis non consulté)
      rate: 0
    sa:                  # header user_type_header = "sa"
      rate: 100
      period: 1m
    user:
      rate: 20
      period: 1m
    "*":                 # fallback si user_type absent ou non listé
      rate: 10
      period: 1m

services:
  - type: audio
    model: "whisper-large-v3"
    default: true           # modèle utilisé par défaut si non précisé et plusieurs modèles configurés
    operations:
      transcription:
        - "/v1/audio/transcriptions"
      translation:
        - "/v1/audio/translations"
    inference_url: "http://GatewAI-transcription-predictor.default.svc.cluster.local"
    accepted_exts: [".mp3", ".wav", ".m4a", ".ogg", ".flac"]
    max_file_size_mb: 500

  - type: ocr
    model: "deepseek-ocr"
    default: true
    operations:
      ocr:
        - "/v1/ocr"
        - "/v1/vision/ocr"    # alias — toutes les paths d'une opération sont indexées
    inference_url: "http://GatewAI-ocr-predictor.default.svc.cluster.local"
    accepted_exts: [".pdf", ".jpg", ".jpeg", ".png", ".tiff", ".bmp"]
    max_file_size_mb: 50

  # Service sync-direct uniquement — POST /v1/* → proxy direct vers inference_url.
  # POST /jobs/{service_type} → 405 Method Not Allowed.
  - type: reranker
    model: "bge-reranker-v2-m3"
    operations:
      rerank:
        - "/rerank"
    inference_url: "http://GatewAI-reranker-predictor.default.svc.cluster.local"
    # Pas de async (pas de relay queue) → sync-direct uniquement

  # LLM proxy — openai, anthropic, ollama ou passthrough (vLLM…)
  # Les requêtes JSON POST /v1/* passent par le proxy LLM (cache, métriques, traduction).
  - type: llm
    model: "chat-smart"                # alias client-facing
    provider: passthrough              # openai | anthropic | ollama | passthrough
    backend_model: "meta-llama/Meta-Llama-3-8B-Instruct"  # vide = alias transmis tel quel
    response_cache_ttl: 3600           # secondes; 0 = désactivé
    operations:
      chat:
        - "/v1/*"                      # wildcard : toutes les paths OpenAI-compatibles
    # Multi-backend : blue/green, canary, fallback
    # weight > 0 = sélection weighted-random ; weight = 0 = fallback uniquement
    backends:
      - url: "http://vllm-primary.default.svc.cluster.local:8000"
        weight: 90
        model: "meta-llama/Meta-Llama-3-8B-Instruct"
        headers:
          Authorization: "Bearer ${VLLM_PRIMARY_TOKEN}"
      - url: "http://vllm-canary.default.svc.cluster.local:8000"
        weight: 10
        model: "meta-llama/Meta-Llama-3.1-8B-Instruct"
        headers:
          Authorization: "Bearer ${VLLM_CANARY_TOKEN}"
    # inference_url: "" (legacy — un seul backend, remplacé par backends[])
    # inference_headers s'applique à tous les backends ; backends[].headers les surcharge
```

#### Champs `services[]`

| Champ | Description |
|---|---|
| `type` | Nom du type de service (ex: `audio`, `ocr`). Plusieurs entrées peuvent partager le même type avec des modèles différents. |
| `model` | Identifiant du modèle, transmis dans le payload OpenAI pour le routage. |
| `default` | `true` → modèle par défaut pour ce type quand aucun `model` n'est précisé dans la requête. |
| `operations` | Map `nom_opération → liste de paths URL`. Tous les paths sont indexés pour le routage sync ; le premier est utilisé comme `inference_url` dans les jobs async. |
| `inference_url` | URL de base du backend pour le direct proxy. Le chemin de la requête d'origine y est appendé. |
| `accepted_exts` | Extensions acceptées (mode async uniquement). Vide ou absent = toutes les extensions acceptées. |
| `max_file_size_mb` | Taille max du fichier. Absent ou 0 = 100 MB par défaut. |
| `inference_url` | URL de base du backend (un seul backend, legacy). Ignoré si `backends` est défini. |
| `backends` | Liste de backends avec routing pondéré. Prend le pas sur `inference_url`. Voir ci-dessous. |
| `inference_headers` | Headers HTTP injectés sur chaque requête vers le backend (sync-direct et LLM proxy). Supporte `${VAR}`. Surchargés par `backends[].headers`. |
| `provider` | Active le LLM proxy : `openai`, `anthropic`, `ollama`, `passthrough`. Absent = proxy direct classique. |
| `backend_model` | Nom du modèle transmis au backend (défaut pour tous les backends). Surchargé par `backends[].model`. |
| `response_cache_ttl` | TTL du cache Redis en secondes. `0` = désactivé. |
| `swagger_url` | URL vers le spec OpenAPI JSON du service. Optionnel — si absent, le service n'apparaît pas dans le dropdown `/docs`. |

#### Champs `backends[]`

| Champ | Description |
|---|---|
| `url` | URL du backend (**requis**) |
| `weight` | Poids de routage. `0` = fallback uniquement (jamais sélectionné en primaire). |
| `model` | Surcharge `backend_model` pour ce backend uniquement — utile pour les déploiements canary. |
| `headers` | Headers HTTP injectés sur les requêtes vers ce backend. Surchargent `inference_headers`. |

### Relay (`relay/config.yaml`)

```yaml
redis:
  addr: "${REDIS_ADDR:-redis:6379}"
  password: "${REDIS_PASSWORD:-}"
  db: 0

model: "${RELAY_MODEL}"          # ex: whisper-large-v3  — sets relay:<model>:pending queue name

queue_pop_timeout: "${QUEUE_POP_TIMEOUT:-5m}"   # BLMOVE timeout before the pod exits 0

s3:
  endpoint:   "${S3_ENDPOINT:-https://s3.fr-par.scw.cloud}"
  region:     "${S3_REGION:-fr-par}"
  access_key: "${S3_ACCESS_KEY}"
  secret_key: "${S3_SECRET_KEY}"
  bucket:     "${S3_BUCKET:-GatewAI-jobs}"

encryption:
  key: "${ENCRYPTION_KEY:-}"

# URL de base du container d'inférence local (même pod).
# Le chemin OpenAI est fourni par le gateway dans InputEvent.inference_url
# et appendé dynamiquement : base_url + inference_url.
inference:
  base_url: "http://127.0.0.1:${INFERENCE_PORT:-9000}"
  api_key:  ""
  timeout:  "300s"
  extra_fields:             # champs form optionnels ajoutés à chaque requête multipart
    response_format: "json"
    # language: "fr"
    # prompt: "..."
```

### Variables d'environnement (gateway)

| Variable | Valeur par défaut | Description |
|---|---|---|
| `CONFIG_PATH` | `config.yaml` | Chemin vers le fichier de configuration |
| `S3_ENDPOINT` | `https://s3.fr-par.scw.cloud` | Endpoint S3 |
| `S3_REGION` | `fr-par` | Région |
| `S3_ACCESS_KEY` | — | Access Key ID (**requis**) |
| `S3_SECRET_KEY` | — | Secret Key (**requis**) |
| `S3_BUCKET` | `GatewAI-jobs` | Nom du bucket |
| `REDIS_ADDR` | `redis:6379` | Adresse Redis |
| `REDIS_PASSWORD` | _(vide)_ | Mot de passe Redis |
| `ENCRYPTION_KEY` | _(vide)_ | Clé AES-256-GCM hex-encodée (32 octets) |
| `CONSUMER_HEADER` | _(vide)_ | Header HTTP pour identifier le consumer (ex: `X-Consumer-Username`) |
| `PRIORITY_HEADER` | _(vide)_ | Header HTTP pour le routing prioritaire (ex: `X-Priority`) |
| `USER_TYPE_HEADER` | _(vide)_ | Header HTTP pour le type d'utilisateur (ex: `X-User-Type`) — rate limiting + métriques LLM |

### Variables d'environnement (relay)

| Variable | Valeur par défaut | Description |
|---|---|---|
| `CONFIG_PATH` | `config.yaml` | Chemin vers le fichier de configuration |
| `RELAY_MODEL` | — | Nom du modèle (**requis**) — détermine la queue `relay:<model>:pending` |
| `QUEUE_POP_TIMEOUT` | `5m` | Timeout BLMOVE avant que le pod quitte avec code 0 |
| `INFERENCE_PORT` | `9000` | Port du serveur de modèle local |
| `REDIS_ADDR` | `redis:6379` | Adresse Redis |
| `REDIS_PASSWORD` | _(vide)_ | Mot de passe Redis |
| `S3_ACCESS_KEY` | — | Access Key ID (**requis**) |
| `S3_SECRET_KEY` | — | Secret Key (**requis**) |
| `ENCRYPTION_KEY` | _(vide)_ | Doit correspondre à la valeur du gateway |

---

## Helm — déploiement du gateway

```bash
helm upgrade --install gatewai-gateway ./helm/gateway \
  -f values.yaml \
  --namespace default
```

### Valeurs clés (`values.yaml`)

```yaml
image:
  repository: ghcr.io/qatr-io/gatewai/gateway
  tag: v0.14.0

config:
  redis:
    addr: "redis:6379"
  s3:
    endpoint: "https://s3.fr-par.scw.cloud"
    bucket: "GatewAI-jobs"

services:
  - type: audio
    model: "whisper-large-v3"
    default: true
    operations:
      transcription:
        - "/v1/audio/transcriptions"
      translation:
        - "/v1/audio/translations"
    inferenceURL: "http://GatewAI-transcription-predictor.default.svc.cluster.local"
    acceptedExts: [".mp3", ".wav", ".m4a", ".ogg", ".flac"]
    maxFileSizeMB: 500
```

---

## Ajouter un service d'inférence

Aucun changement de code n'est nécessaire. Il suffit d'ajouter un bloc dans `config.yaml` (gateway) et de déployer un Relay Deployment configuré avec le bon `model`.

**Gateway `config.yaml`** (service async + sync) :

```yaml
services:
  - type: audio
    model: "pyannote-audio-3.1"
    operations:
      diarization:
        - "/v1/audio/diarizations"
    inference_url: "http://GatewAI-diarization-predictor.default.svc.cluster.local"
    accepted_exts: [".mp3", ".wav", ".m4a", ".ogg", ".flac"]
    max_file_size_mb: 500
```

La queue Redis `relay:pyannote-audio-3.1:pending` est créée automatiquement à la première soumission de job. Aucune configuration de topic préalable n'est nécessaire.

**Gateway `config.yaml`** (service sync-direct uniquement) :

```yaml
services:
  - type: reranker
    model: "bge-reranker-v2-m3"
    operations:
      rerank:
        - "/rerank"
    inference_url: "http://GatewAI-reranker-predictor.default.svc.cluster.local"
    # Pas de relay → sync-direct uniquement
    # POST /jobs/reranker → 405  |  POST /rerank → proxy direct
```

**Relay** (`relay/config.yaml` ou ConfigMap) :

```yaml
model: "pyannote-audio-3.1"   # détermine la queue relay:pyannote-audio-3.1:pending
redis:
  addr: "${REDIS_ADDR:-redis:6379}"
```

> **Multi-modèles par type** : plusieurs entrées peuvent partager le même `type` avec des `model` différents. Le gateway sélectionne le backend d'après le champ `model` de la requête. Le champ `default: true` désigne le modèle utilisé si `model` est absent et que plusieurs modèles sont configurés.

> **Multi-opérations par modèle** : un même modèle peut exposer plusieurs opérations (ex: transcription et translation) via `operations`. En mode async, préciser l'opération avec `-F operation=transcription` quand le modèle en propose plusieurs.

> **Service sync-direct** : un service sans relay est traité entièrement en proxy direct — aucune queue Redis n'est créée pour ce type.

---

## API

### Documentation interactive

Le gateway génère le spec OpenAPI 3.0 à chaque démarrage depuis le registre de services :

- **Swagger UI** : `GET /docs` — dropdown multi-specs : gateway (jobs async/sync) + un onglet par service ayant un `swagger_url`
- **Spec gateway** : `GET /openapi.yaml` — spec générée dynamiquement (routes async + sync)
- **Spec service** : `GET /swagger/{type}/{model}` — spec OpenAPI du service d'inférence, mise en cache au démarrage

### Mode sync — Endpoints OpenAI-compatibles

Ces endpoints sont exposés dynamiquement d'après les `operations` configurées dans `config.yaml`.

#### `POST /v1/audio/transcriptions` — Transcription audio

**Content-Type** : `multipart/form-data`

| Champ | Type | Requis | Description |
|---|---|---|---|
| `model` | string | si plusieurs modèles | Ex: `whisper-large-v3`. Optionnel si un seul modèle ou un défaut configuré. |
| `file` | file | oui | Fichier audio (.mp3, .wav, .m4a, .ogg, .flac) |

```bash
curl https://api.GatewAI.example.com/v1/audio/transcriptions \
  -F model=whisper-large-v3 \
  -F file=@interview.wav
```

**Avec le SDK OpenAI Python**

```python
from openai import OpenAI

client = OpenAI(base_url="https://api.GatewAI.example.com", api_key="unused")

with open("interview.wav", "rb") as f:
    transcript = client.audio.transcriptions.create(
        model="whisper-large-v3",
        file=f,
    )
print(transcript.text)
```

---

#### `POST /v1/ocr` — OCR (documents, images)

**Content-Type** : `multipart/form-data`

| Champ | Type | Requis | Description |
|---|---|---|---|
| `model` | string | si plusieurs modèles | Ex: `deepseek-ocr` |
| `file` | file | oui | Document (.pdf, .jpg, .jpeg, .png, .tiff, .bmp) |

```bash
curl https://api.GatewAI.example.com/v1/ocr \
  -F model=deepseek-ocr \
  -F file=@document.pdf
```

---

### Mode async — Jobs

#### `POST /jobs/{service_type}` — Soumettre un job

**Content-Type** : `multipart/form-data`

| Champ | Type | Requis | Description |
|---|---|---|---|
| `model` | string | si plusieurs modèles sans défaut | Ex: `whisper-large-v3`. Optionnel si un seul modèle ou `default: true` configuré. |
| `operation` | string | si plusieurs opérations | Ex: `transcription` ou `translation`. Optionnel si une seule opération pour le modèle. |
| `file` | file | oui | Fichier à traiter |
| `callback_url` | string | non | URL appelée en POST à la complétion du job |

**Réponse** `202 Accepted`

```json
{
  "job_id": "550e8400-e29b-41d4-a716-446655440000",
  "service_type": "audio",
  "model": "whisper-large-v3",
  "status": "pending"
}
```

```bash
# Modèle et opération explicites
curl -X POST http://localhost:8080/jobs/audio \
  -F "model=whisper-large-v3" \
  -F "operation=transcription" \
  -F "file=@interview.wav" \
  -F "callback_url=https://mon-app.example.com/hooks/inference"

# Modèle par défaut, opération unique → champs optionnels
curl -X POST http://localhost:8080/jobs/audio \
  -F "file=@interview.wav"
```

---

#### `GET /jobs/{service_type}/{id}` — Statut d'un job

**Réponse** `200 OK`

```json
{
  "job_id": "550e8400-e29b-41d4-a716-446655440000",
  "service_type": "audio",
  "model": "whisper-large-v3",
  "status": "completed",
  "result": { "text": "Bonjour, bienvenue à cette réunion..." },
  "created_at": "2026-03-05T10:00:00Z",
  "updated_at": "2026-03-05T10:04:32Z"
}
```

Exemple pour un job en attente :

```json
{
  "job_id": "550e8400-e29b-41d4-a716-446655440000",
  "service_type": "audio",
  "model": "whisper-large-v3",
  "status": "pending",
  "queue_position": 3,
  "created_at": "2026-03-05T10:00:00Z",
  "updated_at": "2026-03-05T10:00:00Z"
}
```

| Champ | Description |
|---|---|
| `status` | `pending` \| `processing` \| `completed` \| `failed` |
| `queue_position` | Position 1-indexée dans la file d'attente du modèle (présent uniquement si `pending`) |
| `result` | Payload JSON du résultat d'inférence (présent uniquement si `completed`) |
| `error` | Message d'erreur (présent uniquement si `failed`) |

> **Attention** : le fichier résultat S3 est supprimé après cet appel — les appels suivants retournent 404.

> **Isolation consumer** : si `consumer_header` est configuré et que le header est présent dans la requête, le job doit appartenir au consumer identifié — sinon `404` (aucune fuite d'information sur les jobs d'autres consumers). Les appels sans header (admin, usage interne) ne sont pas soumis à cette vérification.

---

#### `GET /jobs` — Liste des jobs d'un consumer

Nécessite `consumer_header` configuré. Retourne la liste paginée des jobs du consumer identifié par le header, triée par date de création décroissante.

**Query params** : `limit` (défaut 20, max 100), `offset` (défaut 0)

**Réponse** `200 OK`

```json
{
  "consumer": "alice",
  "total": 42,
  "limit": 20,
  "offset": 0,
  "jobs": [
    {
      "job_id": "550e8400-...",
      "service_type": "audio",
      "model": "whisper-large-v3",
      "status": "completed",
      "created_at": "2026-03-05T10:04:32Z",
      "updated_at": "2026-03-05T10:04:32Z"
    }
  ]
}
```

```bash
curl http://localhost:8080/jobs \
  -H "X-Consumer-Username: alice" \
  "?limit=10&offset=0"
```

> Si `consumer_header` n'est pas configuré, retourne `501 Not Implemented`.

**Polling simple**

```bash
JOB_ID="550e8400-e29b-41d4-a716-446655440000"
while true; do
  RESPONSE=$(curl -s http://localhost:8080/jobs/audio/$JOB_ID)
  STATUS=$(echo $RESPONSE | jq -r '.status')
  [ "$STATUS" = "completed" ] && echo $RESPONSE | jq '.result' && break
  [ "$STATUS" = "failed" ]    && echo "Erreur : $(echo $RESPONSE | jq -r '.error')" && break
  sleep 10
done
```

---

### `GET /health`

```json
{ "status": "ok", "time": "2026-03-05T10:00:00Z" }
```

---

### `GET /metrics`

Métriques Prometheus au format text (scraping compatible avec Prometheus / VictoriaMetrics).

---

## Contrat async (queue Redis)

Le gateway et le relay communiquent via Redis. Les données du job sont stockées dans Redis JSON ; les fichiers d'entrée/sortie sont dans S3.

### Job record — stocké par le gateway dans Redis

```json
{
  "job_id": "550e8400-e29b-41d4-a716-446655440000",
  "service_type": "audio",
  "model": "whisper-large-v3",
  "status": "pending",
  "input_ref": "550e8400-.../input.wav",
  "inference_url": "/v1/audio/transcriptions",
  "created_at": "2026-03-05T10:00:00Z"
}
```

| Champ | Description |
|---|---|
| `input_ref` | Clé objet S3 du fichier d'entrée |
| `inference_url` | Chemin à appeler sur le modèle local (appendé à `inference.base_url` du relay) — dérivé du premier path de l'opération choisie |

### Séquence de file d'attente

```
Gateway   RPUSH relay:<model>:pending <job_id>
Relay     BLMOVE relay:<model>:pending relay:<model>:processing LEFT RIGHT
Relay     [traite le job]
Relay     HSET job:<id> status completed result_ref ...
Relay     PUBLISH jobs:<model>:completed <job_id>
Relay     LREM relay:<model>:processing 1 <job_id>
Gateway   [reçoit PUBLISH, met à jour Redis, déclenche webhook]
```

---

## Webhook (optionnel, mode async)

Si `callback_url` est fourni à la soumission, le gateway effectue un `POST` sur cette URL dès que le job passe à l'état `completed` ou `failed`. En cas d'échec HTTP (5xx ou timeout), 3 tentatives sont faites avec un backoff exponentiel (2 s → 4 s → 8 s).

```json
{
  "job_id": "550e8400-e29b-41d4-a716-446655440000",
  "service_type": "audio",
  "status": "completed",
  "result_ref": "550e8400-.../result.json",
  "completed_at": "2026-03-05T10:04:32Z"
}
```

---

## Monitoring

Les deux composants exposent des métriques Prometheus sur `GET /metrics`.

### Gateway

| Métrique | Type | Labels | Description |
|---|---|---|---|
| `GatewAI_requests_total` | counter | `mode`, `service_type`, `model`, `status` | Requêtes traitées (mode `async` ou `sync`, code HTTP en `status`) |
| `GatewAI_request_duration_seconds` | histogram | `mode`, `service_type`, `model` | Latence bout-en-bout du handler |
| `GatewAI_s3_operation_duration_seconds` | histogram | `operation` (upload/get/delete) | Latence des opérations S3 |
| `GatewAI_s3_errors_total` | counter | `operation` | Erreurs S3 |
| `GatewAI_redis_operation_duration_seconds` | histogram | `operation` (save_job/get_job/delete_job/update_job_result/push_queue) | Latence des opérations Redis |
| `GatewAI_redis_errors_total` | counter | `operation` | Erreurs Redis |
| `GatewAI_jobs_by_consumer_total` | counter | `mode`, `service_type`, `model`, `consumer` | Jobs soumis par consumer (uniquement si `consumer_header` configuré) |
| `GatewAI_llm_requests_total` | counter | `service_type`, `model`, `backend_model`, `provider`, `user_type`, `status` | Requêtes LLM proxy |
| `GatewAI_llm_request_duration_seconds` | histogram | `service_type`, `model`, `backend_model`, `provider`, `user_type` | Latence LLM proxy |
| `GatewAI_llm_tokens_total` | counter | `service_type`, `model`, `backend_model`, `user_type`, `type` | Tokens consommés (`prompt`/`completion`) |
| `GatewAI_llm_tokens_per_request` | histogram | `service_type`, `model`, `backend_model`, `user_type` | Distribution des tokens par requête |
| `GatewAI_llm_consumer_tokens_top` | gauge | `consumer`, `user_type`, `type` | Top-N consumers par tokens (Redis, si `metrics.top_consumers > 0`) |
| `GatewAI_cache_hits_total` | counter | `service_type`, `model` | Cache hits LLM |
| `GatewAI_cache_misses_total` | counter | `service_type`, `model` | Cache misses LLM |
| `GatewAI_cache_errors_total` | counter | `service_type`, `model`, `op` | Erreurs cache LLM |
| `GatewAI_ratelimit_requests_total` | counter | `service_type`, `user_type`, `result` | Checks rate limit (`allowed`/`rejected`) |
| `GatewAI_ratelimit_consumer_hits_total` | counter | `service_type`, `user_type` | Consumers ayant dépassé leur limite |
| `GatewAI_ratelimit_errors_total` | counter | `service_type` | Erreurs Redis lors du rate limiting |

### Relay

| Métrique | Type | Labels | Description |
|---|---|---|---|
| `GatewAI_relay_jobs_total` | counter | `service_type`, `status` (completed/failed) | Jobs traités |
| `GatewAI_relay_inference_duration_seconds` | histogram | `service_type` | Durée de l'appel à l'API d'inférence locale |
| `GatewAI_relay_input_size_bytes` | histogram | `service_type` | Taille des fichiers d'entrée téléchargés depuis S3 |
| `GatewAI_relay_s3_operation_duration_seconds` | histogram | `operation` (get/put/delete) | Latence des opérations S3 |
| `GatewAI_relay_s3_errors_total` | counter | `operation` | Erreurs S3 |
| `GatewAI_relay_redis_publish_errors_total` | counter | — | Erreurs de publication Redis pub/sub (jobs completed) |
| `GatewAI_relay_redis_done_errors_total` | counter | — | Erreurs lors de la suppression du job de la processing list |

### Exemple de configuration Prometheus

```yaml
scrape_configs:
  - job_name: gatewai-gateway
    static_configs:
      - targets: ["gatewai-gateway.default.svc.cluster.local:8080"]

  - job_name: gatewai-relay
    static_configs:
      - targets: ["gatewai-relay.default.svc.cluster.local:8080"]
```

---

## Structure du projet

```
.
├── cmd/gateway/main.go          # Point d'entrée — wiring et graceful shutdown
├── internal/
│   ├── config/config.go         # Chargement YAML + expansion des variables d'env
│   ├── model/job.go             # Types partagés : Job, InputEvent, ResultEvent
│   ├── service/registry.go      # Registre config-driven (routing sync + async, défaut par type)
│   ├── storage/
│   │   ├── s3.go                # Client S3 (AWS SDK v2)
│   │   └── redis.go             # Persistance des jobs (JSON blob + TTL) + RPUSH/LPUSH queue
│   ├── consumer/
│   │   └── manager.go           # Subscriptions Redis pub/sub (jobs:<model>:completed)
│   ├── ratelimit/
│   │   └── ratelimit.go         # Fixed-window rate limiting Redis (Lua INCR+EXPIRE)
│   ├── cache/
│   │   ├── cache.go             # Interface Cache + entrée Redis
│   │   ├── key.go               # Clé SHA-256 canonique du body LLM
│   │   └── redis.go             # Implémentation Redis
│   ├── llmproxy/
│   │   ├── handler.go           # LLM proxy : cache → provider → translate → cache-fill async
│   │   └── provider/            # openai, anthropic, ollama, passthrough
│   ├── metrics/
│   │   ├── metrics.go           # Définitions Prometheus (promauto) — GET /metrics
│   │   └── consumer_tracker.go  # ConsumerTracker interface + Redis sorted-set + top-N refresh
│   └── handler/
│       ├── jobs.go              # POST /jobs/{service_type}  •  GET /jobs/{service_type}/{id}
│       ├── sync.go              # POST /v1/*  (direct proxy ou LLM proxy)
│       ├── docs.go              # GET /docs (Swagger UI)  •  GET /openapi.yaml (spec généré dynamiquement)
│       ├── health.go            # GET /health
│       └── middleware.go        # Logger structuré (slog/JSON)
├── relay/                       # Relay Deployment (module Go séparé : gatewai/relay)
│   ├── cmd/relay/main.go
│   ├── internal/
│   │   ├── config/config.go     # Config relay : model, redis, inference.base_url + extra_fields
│   │   ├── queue/               # BLMOVE pop, Publish (pub/sub), Done (remove from processing)
│   │   ├── store/               # GetJob, UpdateJobResult (Redis JSON)
│   │   ├── relay/               # Traitement des jobs async
│   │   ├── metrics/             # Définitions Prometheus relay — GET /metrics
│   │   ├── adapter/             # Adapter multipart générique (model + extra_fields + file)
│   │   └── storage/             # Client S3
│   └── config.yaml              # Config template (env vars expansées au démarrage)
├── helm/gateway/                # Chart Helm du gateway (inclut Redis-HA)
├── k8s/                         # Manifestes Kubernetes (Relay Deployment, KEDA ScaledObject)
├── config.yaml                  # Configuration par défaut du gateway
└── Dockerfile                   # Multi-stage build → image distroless (~10 MB)
```

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for the gitflow, branch conventions, and release process.
