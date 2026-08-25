// Package queue implements a Redis-backed BLMOVE job queue for the relay.
// Jobs are atomically moved from relay:{model}:pending to relay:{model}:processing
// on Pop, and removed from processing via Done after completion.
//
// Crash recovery: on Pop the relay writes a short-lived lease key
// relay:{model}:lease:{jobID} and refreshes it (Heartbeat) while processing.
// If the pod dies mid-job the lease expires, and the gateway-side reaper
// requeues the abandoned job (see internal/storage.ReapOrphanedProcessingJobs).
// Done removes the job from the processing list and deletes its lease.
package queue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// ErrNoJob is returned by Pop when the queue stays empty for the full timeout.
// The pod was created by KEDA before the job was cancelled — caller should exit 0.
var ErrNoJob = errors.New("no job available")

// Queue manages the relay-side Redis lists for one model.
type Queue struct {
	rdb      *redis.Client
	model    string
	owner    string        // identifies this worker in the lease value (e.g. pod name)
	leaseTTL time.Duration // lease key TTL; refreshed every leaseTTL/3 while processing
	pending  string
	proc     string
}

// New creates a Queue for the given model. owner is written as the lease value
// (typically the pod hostname) for debuggability; leaseTTL bounds how long an
// abandoned job stays un-reaped after the worker dies.
func New(rdb *redis.Client, model, owner string, leaseTTL time.Duration) *Queue {
	return &Queue{
		rdb:      rdb,
		model:    model,
		owner:    owner,
		leaseTTL: leaseTTL,
		pending:  "relay:" + model + ":pending",
		proc:     "relay:" + model + ":processing",
	}
}

// leaseKey returns the per-job lease key.
func (q *Queue) leaseKey(jobID string) string {
	return "relay:" + q.model + ":lease:" + jobID
}

// RefreshInterval is how often Heartbeat renews the lease.
func (q *Queue) RefreshInterval() time.Duration {
	iv := q.leaseTTL / 3
	if iv < time.Second {
		iv = time.Second
	}
	return iv
}

// Pop waits up to timeout for a job ID in the pending list, then atomically
// moves it to the processing list using BLMOVE and acquires a processing lease.
// timeout=0 blocks indefinitely. Returns ErrNoJob when the timeout expires
// with an empty queue (job was cancelled before the pod started).
// Returns context.Canceled on SIGTERM.
func (q *Queue) Pop(ctx context.Context, timeout time.Duration) (string, error) {
	val, err := q.rdb.BLMove(ctx, q.pending, q.proc, "LEFT", "RIGHT", timeout).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return "", ErrNoJob
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("blmove %s → %s: %w", q.pending, q.proc, err)
	}
	// Acquire the processing lease. A failure here is non-fatal: the job is
	// already in the processing list and being worked; the Heartbeat will keep
	// retrying the SET. Worst case the reaper requeues it after leaseTTL,
	// yielding at-least-once processing (async jobs tolerate this).
	if err := q.RefreshLease(ctx, val); err != nil {
		slog.Warn("failed to acquire processing lease", "job_id", val, "error", err)
	}
	return val, nil
}

// RefreshLease (re)writes the lease key with a fresh TTL.
func (q *Queue) RefreshLease(ctx context.Context, jobID string) error {
	if err := q.rdb.Set(ctx, q.leaseKey(jobID), q.owner, q.leaseTTL).Err(); err != nil {
		return fmt.Errorf("set lease %q: %w", jobID, err)
	}
	return nil
}

// Heartbeat renews the lease every RefreshInterval until ctx is cancelled.
// Run it in a goroutine for the lifetime of job processing.
func (q *Queue) Heartbeat(ctx context.Context, jobID string) {
	ticker := time.NewTicker(q.RefreshInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Use a bounded context so a refresh can't hang past the interval.
			rc, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := q.RefreshLease(rc, jobID); err != nil {
				slog.Warn("failed to refresh processing lease", "job_id", jobID, "error", err)
			}
			cancel()
		}
	}
}

// Done removes jobID from the processing list and deletes its lease after
// successful or failed completion. Failures here are logged by the caller —
// they do not block job processing.
func (q *Queue) Done(ctx context.Context, jobID string) error {
	pipe := q.rdb.TxPipeline()
	pipe.LRem(ctx, q.proc, 1, jobID)
	pipe.Del(ctx, q.leaseKey(jobID))
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("done %q: %w", jobID, err)
	}
	return nil
}

// Publish notifies the gateway (and any subscriber) that jobID has completed.
// Channel: jobs:{model}:completed
func (q *Queue) Publish(ctx context.Context, jobID string) error {
	channel := "jobs:" + q.model + ":completed"
	if err := q.rdb.Publish(ctx, channel, jobID).Err(); err != nil {
		return fmt.Errorf("publish %s %q: %w", channel, jobID, err)
	}
	return nil
}

// Ping checks the Redis connection. Returns an error if unreachable.
func (q *Queue) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return q.rdb.Ping(ctx).Err()
}
