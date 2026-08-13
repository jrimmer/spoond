package forkd

import (
	"context"
	"testing"
	"time"
)

// TestClientContextCancellationNotCounted verifies a caller-cancelled
// request is not counted as a controller failure: do() returns on
// ctx.Err() without recording a failure, so a client that gives up (or a
// deadline the caller imposes) can never trip the breaker.
func TestClientContextCancellationNotCounted(t *testing.T) {
	m := newMockController()
	c := m.newClient(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for i := 0; i < breakerThreshold; i++ {
		if _, err := c.ListSnapshots(ctx); err == nil {
			t.Fatalf("call %d: expected cancellation error", i+1)
		}
	}
	if c.breaker.open(time.Now()) {
		t.Fatal("breaker opened on caller cancellation")
	}
}

// TestClientTimeoutReturnsWithoutRetry verifies a request timeout is not
// retried (a re-run would duplicate a non-idempotent exec) and counts as
// exactly one failure. The mock is slow (500ms) while the client timeout
// is 100ms, so the first attempt times out and do() returns immediately.
func TestClientTimeoutReturnsWithoutRetry(t *testing.T) {
	m := newMockController()
	m.latency = 500 * time.Millisecond
	c := m.newClientWithTimeout(t, 100*time.Millisecond)

	if _, err := c.ListSnapshots(context.Background()); err == nil {
		t.Fatal("expected timeout error")
	}
	if got := m.callCount(); got != 1 {
		t.Fatalf("request count = %d, want 1 (no retry on timeout)", got)
	}
	// One failure recorded, below threshold — breaker must stay closed.
	if c.breaker.open(time.Now()) {
		t.Fatal("breaker tripped after a single timeout")
	}
}

// TestClientBreakerRecoveryAfterCooldown verifies the full client-level
// trip -> cooldown -> probe-success -> closed cycle without waiting out a
// real cooldown, by driving the breaker clock directly. It confirms the
// client's recordSuccess on a 2xx path resets the breaker to closed.
func TestClientBreakerRecoveryAfterCooldown(t *testing.T) {
	m := newMockController()
	c := m.newClient(t)

	// Trip the breaker with 503s.
	m.mu.Lock()
	m.forceStatus["GET /v1/snapshots"] = 503
	m.mu.Unlock()
	for i := 0; i < breakerThreshold; i++ {
		if _, err := c.ListSnapshots(context.Background()); err == nil {
			t.Fatalf("call %d: expected 503", i+1)
		}
	}
	if !c.breaker.open(time.Now()) {
		t.Fatal("breaker should be open after threshold 503s")
	}

	// Simulate cooldown having already elapsed, then a healthy controller
	// answers 200: the probe is admitted and recordSuccess closes the
	// breaker.
	m.mu.Lock()
	delete(m.forceStatus, "GET /v1/snapshots")
	m.mu.Unlock()
	c.breaker.mu.Lock()
	c.breaker.openUntil = time.Now().Add(-time.Second) // cooldown elapsed
	c.breaker.probeInFlight = false
	c.breaker.mu.Unlock()

	if _, err := c.ListSnapshots(context.Background()); err != nil {
		t.Fatalf("probe after cooldown: %v", err)
	}
	if c.breaker.open(time.Now()) {
		t.Fatal("breaker should be closed after a successful probe")
	}
}
