package runko

import (
	"testing"
	"time"
)

func TestCircuitBreaker_ClosedByDefault(t *testing.T) {
	cb := newCircuitBreaker(3, 1*time.Second)

	if !cb.allow() {
		t.Error("circuit breaker should be closed (allowing) by default")
	}
}

func TestCircuitBreaker_OpensAfterThreshold(t *testing.T) {
	cb := newCircuitBreaker(3, 1*time.Second)

	// Record failures up to threshold.
	for i := 0; i < 3; i++ {
		cb.recordFailure()
	}

	if cb.allow() {
		t.Error("circuit breaker should be open after reaching threshold")
	}
}

func TestCircuitBreaker_ResetOnSuccess(t *testing.T) {
	cb := newCircuitBreaker(3, 1*time.Second)

	cb.recordFailure()
	cb.recordFailure()
	// 2 failures, then a success should reset.
	cb.recordSuccess()

	// Should still be closed.
	if !cb.allow() {
		t.Error("circuit breaker should reset to closed after success")
	}

	// Need 3 more failures to trip again.
	cb.recordFailure()
	cb.recordFailure()
	if !cb.allow() {
		t.Error("circuit breaker should still be closed with only 2 failures after reset")
	}
}

func TestCircuitBreaker_HalfOpenAfterCooldown(t *testing.T) {
	cb := newCircuitBreaker(2, 50*time.Millisecond)

	cb.recordFailure()
	cb.recordFailure()

	// Should be open.
	if cb.allow() {
		t.Error("circuit breaker should be open")
	}

	// Wait for cooldown.
	time.Sleep(60 * time.Millisecond)

	// First call after cooldown should be allowed (half-open probe).
	if !cb.allow() {
		t.Error("circuit breaker should allow one probe after cooldown")
	}

	// Second call while probing should be rejected.
	if cb.allow() {
		t.Error("circuit breaker should reject additional requests during half-open probe")
	}
}

func TestCircuitBreaker_HalfOpen_SuccessCloses(t *testing.T) {
	cb := newCircuitBreaker(2, 50*time.Millisecond)

	cb.recordFailure()
	cb.recordFailure()

	time.Sleep(60 * time.Millisecond)

	// Probe allowed.
	cb.allow()

	// Probe succeeds — circuit should close.
	cb.recordSuccess()

	if !cb.allow() {
		t.Error("circuit breaker should be closed after successful probe")
	}
}

func TestCircuitBreaker_HalfOpen_FailureReopens(t *testing.T) {
	cb := newCircuitBreaker(2, 50*time.Millisecond)

	cb.recordFailure()
	cb.recordFailure()

	time.Sleep(60 * time.Millisecond)

	// Probe allowed.
	cb.allow()

	// Probe fails — circuit should reopen.
	cb.recordFailure()

	if cb.allow() {
		t.Error("circuit breaker should reopen after failed probe")
	}
}
