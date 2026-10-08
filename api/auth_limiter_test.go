package api

import (
	"testing"
	"time"
)

// TestAuthFailLimiterPrunesStaleIPs pins the L2 leak fix: an IP whose
// failures have all aged out is dropped from the map even without a
// successful auth from that IP, so the limiter does not keep one entry
// per failing IP for the life of the process.
func TestAuthFailLimiterPrunesStaleIPs(t *testing.T) {
	l := newAuthFailLimiter()
	l.window = 10 * time.Millisecond
	now := time.Now()

	// Seed a batch of stale entries directly: every one is older than the
	// window and no success will ever clear them.
	l.mu.Lock()
	for _, ip := range []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"} {
		l.fails[ip] = []time.Time{now.Add(-time.Hour)}
	}
	l.lastPrune = now.Add(-time.Hour)
	l.mu.Unlock()

	// One new failure of a fresh IP triggers the sweep.
	l.hit("10.0.0.9")

	l.mu.Lock()
	defer l.mu.Unlock()
	for _, ip := range []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"} {
		if _, ok := l.fails[ip]; ok {
			t.Errorf("stale IP %s survived the prune", ip)
		}
	}
	if _, ok := l.fails["10.0.0.9"]; !ok {
		t.Errorf("fresh IP was pruned")
	}
}

// TestAuthFailLimiterStillThrottles: the prune does not disturb the hit
// accounting — an IP over the limit is still throttled, and clear()
// resets it.
func TestAuthFailLimiterStillThrottles(t *testing.T) {
	l := newAuthFailLimiter()
	for i := 0; i < l.maxHits; i++ {
		if l.hit("10.0.0.1") {
			t.Fatalf("throttled after %d hits, want false", i+1)
		}
	}
	if !l.hit("10.0.0.1") {
		t.Fatalf("not throttled after %d hits", l.maxHits+1)
	}
	l.clear("10.0.0.1")
	if l.hit("10.0.0.1") {
		t.Errorf("throttled after clear")
	}
}
