# KeventRelayJobFailureRate

**Severity:** warning / critical
**Component:** Relay sidecar

## Meaning

More than 10% (warning) or 30% (critical) of jobs processed by relay sidecars are failing.

## Impact

Async jobs complete with status `failed`. The gateway notifies clients via webhook (if configured) with the error. Clients polling `GET /jobs/{type}/{id}` receive `status: failed` with an error message.

## Diagnosis

```bash
# Failure rate by service_type
sum by (service_type) (rate(GatewAI_relay_jobs_total{status="failed"}[5m]))
  /
sum by (service_type) (rate(GatewAI_relay_jobs_total[5m]))

# Relay pod logs (check for inference errors)
kubectl logs -l serving.kserve.io/inferenceservice=<name> -c kserve-container -n <namespace> --tail=100

# Relay S3 errors (download failures)
increase(GatewAI_relay_s3_errors_total[5m])

# Inference duration — timeout issues?
histogram_quantile(0.95, rate(GatewAI_relay_inference_duration_seconds_bucket[5m]))
```

Common causes:
- Inference model returning non-2xx (malformed input, OOM, model crash)
- S3 download failure (input file missing or corrupted)
- Inference timeout (`inference.timeout` in relay config)
- GPU OOM — model pod restarted mid-job

## Mitigation

1. Check relay logs for the specific error (`inference error`, `s3 download failed`)
2. If the model is crashing: check GPU memory usage, reduce `containerConcurrency`
3. If timeouts: increase `inference.timeout` in relay config, or investigate model performance
4. If S3 errors: check S3 credentials and encryption key consistency between gateway and relay
5. Resubmit failed jobs via `POST /jobs/{service_type}` — results are cleaned up after TTL (72h default)
