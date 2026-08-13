package forkd

import (
	"fmt"
	"sync"
	"time"
)

// defaultTimeout is the controller HTTP client timeout. It is bounded
// well below the previous 600s so a wedged controller cannot hold a
// request for many minutes, while staying above the 300s max exec
// timeout (issue #53).
const defaultTimeout = 330 * time.Second

const (
	breakerThreshold   = 5
	breakerCooldown    = 10 * time.Second
	breakerMaxCooldown = 2 * time.Minute
)

// CircuitOpenError indicates the circuit breaker is open and the
// controller is being skipped so callers fail fast instead of blocking
// on a wedged controller.
type CircuitOpenError struct {
	RetryAfter time.Duration
}

func (e *CircuitOpenError) Error() string {
	return fmt.Sprintf("forkd: circuit open (controller unhealthy); retry after %s", e.RetryAfter.Round(time.Millisecond))
}

// circuitBreaker trips after threshold consecutive failures and stays
// open for a cooldown that grows exponentially on repeated trips
// (issue #53). Once the cooldown elapses it admits exactly one probe
// (single-flight half-open): if that probe fails the breaker re-trips
// immediately, if it succeeds it resets to closed.
type circuitBreaker struct {
	mu               sync.Mutex
	consecutiveFails int
	openUntil        time.Time // zero = closed
	probeInFlight    bool      // a half-open probe has been admitted
	cooldown         time.Duration
	threshold        int
	baseCooldown     time.Duration
	maxCooldown      time.Duration
}

func newCircuitBreaker(threshold int, baseCooldown, maxCooldown time.Duration) *circuitBreaker {
	return &circuitBreaker{
		threshold:    threshold,
		baseCooldown: baseCooldown,
		maxCooldown:  maxCooldown,
		cooldown:     baseCooldown,
	}
}

// allow admits a call, or returns CircuitOpenError when the breaker is
// open or a probe is already in flight (single-flight half-open).
func (b *circuitBreaker) allow(now time.Time) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch {
	case b.probeInFlight:
		return &CircuitOpenError{RetryAfter: b.cooldown}
	case b.openUntil.After(now):
		return &CircuitOpenError{RetryAfter: b.openUntil.Sub(now)}
	case !b.openUntil.IsZero():
		// Half-open: the cooldown has elapsed; admit exactly one probe.
		b.probeInFlight = true
		return nil
	default:
		return nil // closed
	}
}

// recordFailure counts a failed controller call. Failures that arrive
// while the breaker is already open (stragglers admitted before the
// trip) are ignored so they cannot re-escalate the backoff; a failing
// half-open probe re-trips and grows the cooldown.
func (b *circuitBreaker) recordFailure(now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.probeInFlight = false
	if b.openUntil.After(now) {
		return // straggler: breaker already open, do not re-escalate
	}
	b.consecutiveFails++
	if b.consecutiveFails >= b.threshold {
		b.openUntil = now.Add(b.cooldown)
		// Exponential backoff on repeated trips.
		if b.cooldown < b.maxCooldown {
			b.cooldown *= 2
			if b.cooldown > b.maxCooldown {
				b.cooldown = b.maxCooldown
			}
		}
		b.consecutiveFails = b.threshold
	}
}

// recordSuccess closes the breaker and resets the backoff to base.
func (b *circuitBreaker) recordSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.probeInFlight = false
	b.consecutiveFails = 0
	b.openUntil = time.Time{}
	b.cooldown = b.baseCooldown
}

// open reports whether the breaker is currently open (within cooldown).
func (b *circuitBreaker) open(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.openUntil.After(now)
}
