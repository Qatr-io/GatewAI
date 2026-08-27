---
title: Async flow
---

# Relay — async flow

The relay is a sidecar container that runs alongside the inference pod. It pulls jobs from a Redis queue, calls the local inference model, and publishes results back to the gateway.

## Architecture

```
Gateway
  ├── Upload input → S3
  ├── Save job record → Redis (status: pending)
  └── RPUSH job_id → relay:<model>:pending
                              │
                    ┌─────────▼──────────┐
                    │       Relay        │
                    │  (sidecar pod)     │
                    └─────────┬──────────┘
                              │ BLMOVE LEFT RIGHT
                    relay:<model>:pending
                           → relay:<model>:processing
                              │
                    1. Download + decrypt input from S3
                    2. POST to inference (127.0.0.1:9000/...)
                    3. Upload result → S3
                    4. PUBLISH → jobs:<model>:completed
                    5. Remove from relay:<model>:processing
                              │
                    Gateway subscriber
                    ├── Update job status → Redis (completed)
                    ├── Trigger webhook (if callback_url set)
                    └── Notify pub/sub (job:<id>:done)
```

## Job lifecycle

### 1. Startup

The relay polls `GET /health` on the local inference service (port 9000) until it returns `200`. This ensures the inference model is loaded before any jobs are dequeued.

### 2. Dequeue

```
BLMOVE relay:<model>:pending relay:<model>:processing LEFT RIGHT [timeout]
```

Blocks until a job arrives or the `queue_pop_timeout` expires (default: 5 min). On timeout, the relay exits `0` — KEDA will not restart it while the queue remains empty.

On a successful dequeue the relay acquires a **processing lease** — `SET relay:<model>:lease:<id>` with TTL `lease_ttl` (config, default `60s`) — and refreshes it every `lease_ttl/3` for the whole job. If this pod dies mid-job the lease expires, and the gateway reaper requeues the job (see [Crash recovery](../set-up/queue#crash-recovery)). `lease_ttl` need not be sized to the job length — the heartbeat keeps it alive for arbitrarily long jobs; it only bounds how quickly a *dead* pod's job is detected.

### 3. Process

1. Fetch job record from Redis (service type, model, S3 key, operation)
2. Download input file from S3
3. Decrypt if encryption is enabled (AES-256-GCM streaming, see [Encryption](../configure/encryption))
4. `POST` plaintext to `http://127.0.0.1:9000/<inference_url>` (multipart)
5. Upload `result.json` to S3

### 4. Complete

```
PUBLISH jobs:<model>:completed  {"job_id": "...", "status": "completed", ...}
```

The gateway subscriber receives this, updates the job record in Redis, and triggers the callback webhook if configured.

### 5. Exit

The relay removes the job from `relay:<model>:processing` **and deletes its lease**, then exits `0`. KEDA creates a fresh pod for the next job.

## Priority jobs

When `server.priority_header` is set and the header is present on the submission request, the gateway uses `LPUSH` instead of `RPUSH`. The relay always dequeues from the left (`BLMOVE LEFT RIGHT`), so priority jobs skip ahead of the queue with no separate Deployment required.

## Cancellation

When a client calls `DELETE /jobs/{service_type}/{id}`, the gateway publishes to the `relay:<model>:cancel` channel. The relay subscribes to this channel while processing; if it receives the signal, it interrupts inference and marks the job as cancelled. The relay then exits `0`.

Jobs cancelled before processing begins are simply marked cancelled in Redis — no relay interaction needed.

## Error handling

| Scenario | Behaviour |
|----------|-----------|
| Inference returns non-200 | Job marked `failed`, error stored in Redis |
| S3 download fails | Job marked `failed` |
| S3 upload fails | Job marked `failed` |
| Relay pod crashes (no `Done`) | Lease expires; the gateway reaper requeues the job automatically (see [Crash recovery](../set-up/queue#crash-recovery)) — dead-lettered after `gc.max_reap_attempts` |
| Relay exits on BLMOVE timeout | Pod exits `0`, KEDA does not restart until queue is non-empty |

A crashed pod's job is recovered automatically (lease + reaper). Terminally-`failed` jobs (bad input, inference error) are **not** retried — resubmit via `POST /jobs/{service_type}` if needed.

## Redis key schema

| Key | Type | Description |
|-----|------|-------------|
| `relay:<model>:pending` | list | Jobs waiting to be picked up |
| `relay:<model>:processing` | list | Job currently being processed (at most one per pod) |
| `relay:<model>:lease:<id>` | string (TTL) | Processing lease, refreshed while the pod works |
| `relay:<model>:deadletter` | list | Jobs the reaper could not recover |
| `jobs:<model>:completed` | pub/sub | Relay → gateway completion signal |
| `relay:<model>:cancel` | pub/sub | Gateway → relay cancellation signal |
| `job:<id>:done` | pub/sub | Gateway → polling clients notification |
