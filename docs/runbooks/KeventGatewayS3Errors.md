# KeventGatewayS3Errors

**Severity:** warning
**Component:** Gateway

## Meaning

The gateway encountered S3 errors (upload, get, or delete) in the last 5 minutes.

## Impact

- Upload errors: async job submissions fail at the file storage step (500 returned to client)
- Get errors: completed job results cannot be fetched (`GET /jobs/{type}/{id}` returns incomplete data)
- Delete errors: orphaned files accumulate in S3 (no functional impact, storage cost)

## Diagnosis

```bash
# Errors by operation
sum by (operation) (increase(GatewAI_s3_errors_total[5m]))

# Gateway logs
kubectl logs -l app.kubernetes.io/name=gatewai-gateway -n <namespace> | grep -E "s3 (upload|result fetch) failed"

# Check S3 reachability
kubectl exec -it <gateway-pod> -n <namespace> -- curl -I <s3-endpoint>
```

Common causes:
- S3 credentials expired or incorrect
- Bucket does not exist or wrong region configured
- Network connectivity issue to the S3 endpoint
- S3 service outage

## Mitigation

1. Verify S3 credentials in the secret (`S3_ACCESS_KEY`, `S3_SECRET_KEY`)
2. Check endpoint and region in gateway config
3. Confirm bucket exists and the credentials have read/write access
4. If encryption is enabled, verify `ENCRYPTION_KEY` matches between gateway and relay
