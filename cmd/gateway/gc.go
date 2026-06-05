package main

import (
	"context"
	"log/slog"
	"time"

	"gatewai/gateway/internal/metrics"
	"gatewai/gateway/internal/storage"
)

// runGC executes one GC cycle:
//   - Phase 1: sweep stale-pending jobs (skipped when maxAge == 0)
//   - Phase 2: delete orphaned S3 objects for jobs absent from Redis
//
// Phase 2 aborts early if Redis is unavailable or returns errors, to avoid
// deleting S3 objects whose jobs simply couldn't be verified.
func runGC(ctx context.Context, redis *storage.RedisClient, s3Client *storage.S3Client, maxAge, orphanMinAge time.Duration) {
	// ── Phase 1: stale-pending sweep ─────────────────────────────────────────
	if maxAge > 0 {
		swept, err := redis.SweepStalePendingJobs(ctx, maxAge)
		if err != nil {
			slog.Error("GC phase1: stale-pending sweep failed", "error", err)
		} else {
			for _, job := range swept {
				metrics.AsyncStaleJobsSweptTotal.WithLabelValues(job.Model).Inc()
				if job.InputRef == "" {
					continue
				}
				inputRef := job.InputRef
				jobID := job.ID
				go func() {
					if err := s3Client.DeleteObject(context.Background(), inputRef); err != nil {
						slog.Error("GC phase1: failed to delete stale input", "job_id", jobID, "input_ref", inputRef, "error", err)
					}
				}()
			}
			if len(swept) > 0 {
				slog.Info("GC phase1: swept stale-pending jobs", "count", len(swept))
			}
		}
	}

	// ── Phase 2: orphan S3 cleanup ────────────────────────────────────────────
	// Safeguard: abort if Redis is unavailable — we cannot distinguish "job gone"
	// from "Redis down" without a reliable ping.
	if err := redis.Ping(ctx); err != nil {
		slog.Error("GC phase2: Redis unavailable, skipping S3 orphan cleanup", "error", err)
		return
	}

	jobObjects, err := s3Client.ListJobObjects(ctx)
	if err != nil {
		slog.Error("GC phase2: S3 listing failed, skipping orphan cleanup", "error", err)
		return
	}

	// Collect job IDs whose oldest S3 object predates the orphan_min_age window.
	var candidates []string
	for jobID, entry := range jobObjects {
		if time.Since(entry.OldestModTime) >= orphanMinAge {
			candidates = append(candidates, jobID)
		}
	}
	if len(candidates) == 0 {
		return
	}

	exists, err := redis.JobsExistBatch(ctx, candidates)
	if err != nil {
		slog.Error("GC phase2: Redis inventory unreliable, skipping S3 orphan cleanup", "error", err)
		return
	}

	orphans := 0
	for _, jobID := range candidates {
		if exists[jobID] {
			continue
		}
		orphans++
		for _, key := range jobObjects[jobID].Keys {
			if err := s3Client.DeleteObject(ctx, key); err != nil {
				slog.Error("GC phase2: failed to delete orphan S3 object", "job_id", jobID, "key", key, "error", err)
			} else {
				slog.Info("GC phase2: deleted orphan S3 object", "job_id", jobID, "key", key)
			}
		}
	}
	if orphans > 0 {
		slog.Info("GC phase2: orphan S3 cleanup complete", "orphans", orphans)
	}
}
