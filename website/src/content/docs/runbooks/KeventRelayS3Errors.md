---
title: KeventRelayS3Errors
---

# KeventRelayS3Errors

**Severity:** warning
**Component:** Relay sidecar

## Meaning

Relay sidecars encountered S3 errors (get/put/delete) in the last 5 minutes.

## Impact

- Get errors: relay cannot download the input file → job marked `failed`
- Put errors: inference result cannot be uploaded → job marked `failed`, result lost
- Delete errors: input file not cleaned up after processing (storage cost, no functional impact)

## Diagnosis

```bash
# Errors by operation
sum by (operation) (increase(GatewAI_relay_s3_errors_total[5m]))

# Relay logs
kubectl logs -l serving.kserve.io/inferenceservice=<name> -c kserve-container -n <namespace> \
  | grep -E "s3|S3"

# Check if gateway S3 errors also firing (shared credentials issue)
increase(GatewAI_s3_errors_total[5m])
```

Common causes:
- S3 credentials not injected into relay pod (`S3_ACCESS_KEY`, `S3_SECRET_KEY` env vars)
- Encryption key mismatch between gateway (writes) and relay (reads) — `ENCRYPTION_KEY` must be identical
- Input file deleted before relay could download it (TTL issue or duplicate job)
- S3 endpoint unreachable from the inference pod's network namespace

## Mitigation

1. Verify S3 env vars are set in the KServe ServingRuntime/InferenceService manifest
2. Confirm `ENCRYPTION_KEY` is the same secret referenced in both gateway and relay deployments
3. Check network policies — inference pods must reach the S3 endpoint
4. If input files are disappearing prematurely: check `redis.pending_max_age` and `lifecycle.job_ttl.pending` vs inference duration
