---
title: Relay queue (Redis)
---

# Relay queue (Redis)

## Overview

Async jobs are routed through Redis lists instead of Kafka. The gateway pushes job IDs onto a per-model list; each relay pod pops one job, processes it, and exits.

## Redis key schema

| Key | Type | Operation | Description |
|---|---|---|---|
| `relay:<model>:pending` | list | RPUSH (normal) / LPUSH (priority) | Jobs waiting to be picked up |
| `relay:<model>:processing` | list | BLMOVE destination | Job currently being processed by a pod |
| `jobs:<model>:completed` | pub/sub channel | PUBLISH | Relay signals completion to the gateway |
| `relay:<model>:cancel` | pub/sub channel | PUBLISH | Gateway signals job cancellation to the relay |

## Priority routing

When `server.priority_header` is set and the header is present in the request, the gateway uses `LPUSH` instead of `RPUSH`. The relay always pops from the left (`BLMOVE LEFT RIGHT`), so priority jobs are dequeued first. No separate Deployment is needed.

## KEDA autoscaling

The relay Deployment is scaled by KEDA using the Redis scaler on `relay:<model>:pending` list length:

```yaml
triggers:
  - type: redis
    metadata:
      address: redis:6379
      listName: relay:whisper-large-v3:pending
      listLength: "1"
```

`minReplicaCount: 0` — pods scale to zero when the queue is empty.

## Relay lifecycle

Each relay pod:
1. Polls `/health` on the local inference service until ready
2. Calls `BLMOVE relay:<model>:pending relay:<model>:processing LEFT RIGHT` with a configurable timeout (`queue_pop_timeout`, default 5 min)
3. Exits `0` on timeout (empty queue) or SIGTERM — KEDA will not restart a zero-queue pod
4. Processes the job (download S3 → inference → upload S3 → publish completion)
5. Exits `0` — KEDA creates a fresh pod for the next job

## Stale processing list

If a relay pod crashes mid-job without calling `Done`, the job remains in `relay:<model>:processing`. The GC's stale-pending sweep (`redis.pending_max_age`) does not cover the processing list. To recover a stuck job manually:

```bash
# Inspect processing list
redis-cli LRANGE relay:whisper-large-v3:processing 0 -1

# Move job back to pending (re-queue)
redis-cli LMOVE relay:whisper-large-v3:processing relay:whisper-large-v3:pending RIGHT LEFT
```
