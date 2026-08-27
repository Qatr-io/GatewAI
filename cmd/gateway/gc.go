package main

import (
	"context"
	"log/slog"
	"time"

	"gatewai/gateway/internal/metrics"
	"gatewai/gateway/internal/service"
	"gatewai/gateway/internal/storage"
)

// runGC executes one GC cycle:
//   - Phase 0: reap jobs abandoned in the processing list (dead relay pod)
//   - Phase 1: sweep stale-pending jobs (skipped when maxAge == 0)
//   - Phase 2: delete orphaned S3 objects for jobs absent from Redis
//   - Phase 3: remove relay queue entries for jobs absent from Redis
//
// Phases 2 and 3 abort early if Redis is unavailable, to avoid deleting S3
// objects or queue entries whose jobs simply couldn't be verified.
func runGC(ctx context.Context, redis *storage.RedisClient, s3Client *storage.S3Client, reg *service.Registry, maxAge, orphanMinAge time.Duration, maxReapAttempts int) {
	// ── Phase 0: reap orphaned processing jobs ───────────────────────────────
	// Requeue (or dead-letter) jobs whose relay pod died mid-processing without
	// releasing its lease (live job record). Per-outcome metrics are emitted
	// inside the reaper. Phase 3 below handles entries whose record is already
	// gone; the two are complementary (lease requeue vs record-gone cleanup).
	if res, err := redis.ReapOrphanedProcessingJobs(ctx, maxReapAttempts); err != nil {
		slog.Error("GC phase0: processing-list reaper failed", "error", err)
	} else if res.Requeued+res.DeadLettered+res.Dropped > 0 {
		slog.Info("GC phase0: reaped orphaned processing jobs",
			"requeued", res.Requeued, "deadletter", res.DeadLettered, "dropped", res.Dropped)
	}

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

	// Safeguard: abort phases 2 and 3 if Redis is unavailable — we cannot
	// distinguish "job gone" from "Redis down" without a reliable ping.
	if err := redis.Ping(ctx); err != nil {
		slog.Error("GC: Redis unavailable, skipping orphan cleanup", "error", err)
		return
	}

	// ── Phase 2: orphan S3 cleanup ────────────────────────────────────────────
	runOrphanS3Cleanup(ctx, redis, s3Client, orphanMinAge)

	// ── Phase 3: orphaned relay queue entries ────────────────────────────────
	runOrphanRelayQueueCleanup(ctx, redis, reg)
}

func runOrphanS3Cleanup(ctx context.Context, redis *storage.RedisClient, s3Client *storage.S3Client, orphanMinAge time.Duration) {
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

func runOrphanRelayQueueCleanup(ctx context.Context, redis *storage.RedisClient, reg *service.Registry) {
	if reg == nil {
		return
	}
	seen := make(map[string]struct{})
	var models []string
	for _, def := range reg.All() {
		if !def.SupportsAsync || def.Model == "" {
			continue
		}
		if _, ok := seen[def.Model]; ok {
			continue
		}
		seen[def.Model] = struct{}{}
		models = append(models, def.Model)
	}
	if len(models) == 0 {
		return
	}

	removed, err := redis.SweepOrphanedRelayQueueEntries(ctx, models)
	if err != nil {
		slog.Error("GC phase3: relay queue orphan sweep failed", "error", err)
		return
	}
	total := 0
	for _, r := range removed {
		total += r.Count
		metrics.RelayQueueOrphansSweptTotal.WithLabelValues(r.Model, r.State).Add(float64(r.Count))
	}
	if total > 0 {
		slog.Info("GC phase3: relay queue orphan sweep complete", "removed", total)
	}
}
