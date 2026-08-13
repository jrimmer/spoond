package forkd

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestCircuitBreakerSingleProbeConcurrent verifies the single-flight
// half-open state under real parallelism: after the cooldown elapses,
// exactly one of N concurrent callers is admitted as the probe while the
// rest are rejected.
func TestCircuitBreakerSingleProbeConcurrent(t *testing.T) {
	b := newCircuitBreaker(1, 10*time.Second, time.Minute)
	now := time.Unix(1000, 0)
	b.recordFailure(now) // open
	after := now.Add(10*time.Second + time.Second)

	const n = 64
	var admitted int32
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if b.allow(after) == nil {
				atomic.AddInt32(&admitted, 1)
			}
		}()
	}
	wg.Wait()
	if admitted != 1 {
		t.Fatalf("admitted %d probes, want exactly 1 (single-flight half-open)", admitted)
	}
}

// TestCircuitBreakerParallelFailuresTripOnce verifies that a burst of
// concurrent failures trips the breaker exactly once and escalates the
// cooldown only once: the first threshold failures open the breaker, and
// the remaining in-flight failures are stragglers that are ignored
// rather than each re-escalating the backoff.
func TestCircuitBreakerParallelFailuresTripOnce(t *testing.T) {
	b := newCircuitBreaker(5, 10*time.Second, time.Minute)
	now := time.Unix(1000, 0)

	const n = 20
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			b.recordFailure(now)
		}()
	}
	wg.Wait()

	if !b.open(now) {
		t.Fatal("breaker should be open after threshold failures")
	}
	// Exactly one trip: the cooldown doubled once (10s -> 20s), not
	// escalated toward the 2m cap by the 20 concurrent failures.
	b.mu.Lock()
	cooldown := b.cooldown
	b.mu.Unlock()
	if cooldown != 20*time.Second {
		t.Fatalf("cooldown = %s, want 20s (single trip under concurrent failures)", cooldown)
	}
}
