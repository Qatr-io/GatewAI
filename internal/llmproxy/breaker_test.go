package llmproxy

import (
	"testing"
	"time"
)

func TestCircuitBreaker_OpensAfterThreshold(t *testing.T) {
	cb := NewCircuitBreaker(3, 50*time.Millisecond)
	url := "http://b"

	cb.RecordFailure("m", url)
	cb.RecordFailure("m", url)
	if !cb.Allow(url) {
		t.Fatal("should still allow before the threshold is reached")
	}
	cb.RecordFailure("m", url) // 3rd consecutive → open
	if cb.Allow(url) {
		t.Fatal("circuit should be open after the threshold")
	}
	if !cb.IsOpen(url) {
		t.Fatal("IsOpen should report open")
	}
}

func TestCircuitBreaker_SuccessResetsCount(t *testing.T) {
	cb := NewCircuitBreaker(3, time.Minute)
	url := "http://b"

	cb.RecordFailure("m", url)
	cb.RecordFailure("m", url)
	cb.RecordSuccess("m", url) // reset
	cb.RecordFailure("m", url)
	cb.RecordFailure("m", url)
	if !cb.Allow(url) {
		t.Fatal("a success must reset the consecutive-failure count")
	}
}

func TestCircuitBreaker_HalfOpenProbeAfterCooldown(t *testing.T) {
	cb := NewCircuitBreaker(1, 20*time.Millisecond)
	url := "http://b"

	cb.RecordFailure("m", url) // open immediately (threshold 1)
	if cb.Allow(url) {
		t.Fatal("should be open right after failing")
	}
	time.Sleep(30 * time.Millisecond)
	if !cb.Allow(url) {
		t.Fatal("a probe should be allowed once the cooldown elapses")
	}
	cb.RecordSuccess("m", url) // probe succeeds → close
	if cb.IsOpen(url) {
		t.Fatal("circuit should close after a successful probe")
	}
}

func TestCircuitBreaker_FailedProbeReopens(t *testing.T) {
	cb := NewCircuitBreaker(1, 20*time.Millisecond)
	url := "http://b"

	cb.RecordFailure("m", url)
	time.Sleep(30 * time.Millisecond)
	if !cb.Allow(url) {
		t.Fatal("probe should be allowed after cooldown")
	}
	cb.RecordFailure("m", url) // failed probe → extend the open window
	if cb.Allow(url) {
		t.Fatal("circuit should re-open after a failed probe")
	}
}

func TestCircuitBreaker_UnseenBackendAllowed(t *testing.T) {
	cb := NewCircuitBreaker(3, time.Minute)
	if !cb.Allow("http://never-seen") {
		t.Fatal("an unseen backend must be allowed")
	}
	if cb.IsOpen("http://never-seen") {
		t.Fatal("an unseen backend is not open")
	}
}
