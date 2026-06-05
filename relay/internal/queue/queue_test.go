package queue_test

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"gatewai/relay/internal/queue"
)

func newTestQueue(t *testing.T) (*queue.Queue, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	return queue.New(rdb, "test-model"), mr
}

func listLen(t *testing.T, mr *miniredis.Miniredis, key string) int {
	t.Helper()
	items, err := mr.List(key)
	if err != nil {
		return 0
	}
	return len(items)
}

func listItems(t *testing.T, mr *miniredis.Miniredis, key string) []string {
	t.Helper()
	items, _ := mr.List(key)
	return items
}

// TestPop_MovesJobFromPendingToProcessing verifies that Pop atomically moves
// a job ID from relay:{model}:pending to relay:{model}:processing via BLMOVE.
func TestPop_MovesJobFromPendingToProcessing(t *testing.T) {
	q, mr := newTestQueue(t)
	mr.RPush("relay:test-model:pending", "job-1")

	jobID, err := q.Pop(context.Background(), 5*time.Second)
	if err != nil {
		t.Fatalf("Pop: %v", err)
	}
	if jobID != "job-1" {
		t.Errorf("expected job-1, got %q", jobID)
	}
	if listLen(t, mr, "relay:test-model:pending") != 0 {
		t.Error("pending list should be empty after Pop")
	}
	items := listItems(t, mr, "relay:test-model:processing")
	if len(items) != 1 || items[0] != "job-1" {
		t.Errorf("processing list should contain [job-1], got %v", items)
	}
}

// TestPop_OnlyPopsOncePerCall verifies that a single Pop call moves exactly
// one job — even if multiple jobs are pending — confirming one-job-per-pop.
func TestPop_OnlyPopsOncePerCall(t *testing.T) {
	q, mr := newTestQueue(t)
	mr.RPush("relay:test-model:pending", "job-1", "job-2")

	jobID, err := q.Pop(context.Background(), 5*time.Second)
	if err != nil {
		t.Fatalf("Pop: %v", err)
	}
	if jobID != "job-1" {
		t.Errorf("expected job-1 (FIFO), got %q", jobID)
	}

	// job-2 must stay in pending — Pop must NOT move it to processing.
	if listLen(t, mr, "relay:test-model:pending") != 1 {
		t.Errorf("pending should still have 1 job, got %d", listLen(t, mr, "relay:test-model:pending"))
	}
	if listLen(t, mr, "relay:test-model:processing") != 1 {
		t.Errorf("processing should have exactly 1 job, got %d", listLen(t, mr, "relay:test-model:processing"))
	}
}

// TestPop_DoesNotPickUpJobsAlreadyInProcessing verifies that Pop only reads
// from the pending list. A job stuck in processing (orphaned) is NOT popped
// even when a real pending job arrives — the orphan stays in processing.
// This documents the known behaviour: orphaned jobs require external GC.
func TestPop_DoesNotPickUpJobsAlreadyInProcessing(t *testing.T) {
	q, mr := newTestQueue(t)

	// Orphaned job already in processing, plus one real pending job.
	mr.RPush("relay:test-model:processing", "orphan-job")
	mr.RPush("relay:test-model:pending", "real-job")

	jobID, err := q.Pop(context.Background(), 5*time.Second)
	if err != nil {
		t.Fatalf("Pop: %v", err)
	}
	// Pop must return the pending job, not the orphan.
	if jobID != "real-job" {
		t.Errorf("expected real-job, got %q", jobID)
	}
	// Orphan must be untouched in processing.
	items := listItems(t, mr, "relay:test-model:processing")
	if len(items) != 2 {
		t.Errorf("processing should have orphan-job + real-job, got %v", items)
	}
	found := false
	for _, v := range items {
		if v == "orphan-job" {
			found = true
		}
	}
	if !found {
		t.Error("orphan-job should still be in processing list")
	}
}

// TestDone_RemovesJobFromProcessing verifies that Done removes the job from
// the processing list, leaving it empty.
func TestDone_RemovesJobFromProcessing(t *testing.T) {
	q, mr := newTestQueue(t)
	mr.RPush("relay:test-model:pending", "job-1")
	if _, err := q.Pop(context.Background(), 5*time.Second); err != nil {
		t.Fatalf("Pop: %v", err)
	}

	if err := q.Done(context.Background(), "job-1"); err != nil {
		t.Fatalf("Done: %v", err)
	}
	if listLen(t, mr, "relay:test-model:processing") != 0 {
		t.Error("processing list should be empty after Done")
	}
}

// TestPopThenDone_FullLifecycle verifies the complete happy path:
// pending → processing (Pop) → removed (Done).
// After Done, a second job can be popped without interference.
func TestPopThenDone_FullLifecycle(t *testing.T) {
	q, mr := newTestQueue(t)
	mr.RPush("relay:test-model:pending", "job-1", "job-2")

	id1, err := q.Pop(context.Background(), 5*time.Second)
	if err != nil || id1 != "job-1" {
		t.Fatalf("first Pop: got %q, %v", id1, err)
	}
	if err := q.Done(context.Background(), id1); err != nil {
		t.Fatalf("Done(job-1): %v", err)
	}
	if listLen(t, mr, "relay:test-model:processing") != 0 {
		t.Error("processing should be empty after Done")
	}

	id2, err := q.Pop(context.Background(), 5*time.Second)
	if err != nil || id2 != "job-2" {
		t.Fatalf("second Pop: got %q, %v", id2, err)
	}
	if listLen(t, mr, "relay:test-model:pending") != 0 {
		t.Error("pending should be empty after second Pop")
	}
	if listLen(t, mr, "relay:test-model:processing") != 1 {
		t.Error("processing should have job-2")
	}
}

// TestPop_CancelledContext_ReturnsError verifies that Pop returns an error
// immediately when the context is already cancelled before the call.
func TestPop_CancelledContext_ReturnsError(t *testing.T) {
	q, _ := newTestQueue(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before calling Pop

	done := make(chan error, 1)
	go func() {
		_, err := q.Pop(ctx, 5*time.Second)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error from cancelled context, got nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Pop did not return after context was cancelled")
	}
}
