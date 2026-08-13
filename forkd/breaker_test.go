package forkd

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestCircuitBreakerTripsAfterThreshold(t *testing.T) {
	b := newCircuitBreaker(3, 10*time.Second, time.Minute)
	now := time.Unix(1000, 0)
	for i := 0; i < 2; i++ {
		b.recordFailure(now)
		if b.open(now) {
			t.Fatalf("breaker opened after %d failure(s)", i+1)
		}
	}
	b.recordFailure(now) // 3rd failure trips it
	if !b.open(now) {
		t.Fatal("breaker should be open after threshold failures")
	}
	if _, ok := b.allow(now).(*CircuitOpenError); !ok {
		t.Fatal("allow should reject while open")
	}
}

// TestCircuitBreakerSingleProbe verifies the single-flight half-open
// state: exactly one caller is admitted as the probe after the cooldown
// elapses; concurrent callers are rejected until the probe resolves.
func TestCircuitBreakerSingleProbe(t *testing.T) {
	b := newCircuitBreaker(1, 10*time.Second, time.Minute)
	now := time.Unix(1000, 0)
	b.recordFailure(now) // open
	after := now.Add(10 * time.Second)
	if err := b.allow(after); err != nil {
		t.Fatalf("first probe allow: %v", err)
	}
	if err := b.allow(after); err == nil {
		t.Fatal("second call should be rejected while a probe is in flight")
	}
	b.recordSuccess()
	if err := b.allow(after); err != nil {
		t.Fatalf("allow after probe success: %v", err)
	}
}

func TestCircuitBreakerHalfOpenReTripsAndResets(t *testing.T) {
	b := newCircuitBreaker(2, 10*time.Second, time.Minute)
	now := time.Unix(1000, 0)
	b.recordFailure(now)
	b.recordFailure(now) // open
	if !b.open(now) {
		t.Fatal("breaker should be open")
	}
	after := now.Add(10 * time.Second)
	if err := b.allow(after); err != nil {
		t.Fatalf("half-open allow: %v", err)
	}
	b.recordFailure(after) // failing probe re-trips
	if !b.open(after) {
		t.Fatal("failing half-open probe should re-trip")
	}
	b.recordSuccess() // success closes
	if b.open(after) {
		t.Fatal("success should close the breaker")
	}
}

func TestCircuitBreakerExponentialBackoff(t *testing.T) {
	b := newCircuitBreaker(1, 10*time.Second, time.Minute)
	now := time.Unix(1000, 0)
	b.recordFailure(now) // trip #1: cooldown 10s
	if got := retryAfter(t, b, now); got != 10*time.Second {
		t.Fatalf("first retryAfter = %s, want 10s", got)
	}
	now = now.Add(10*time.Second + time.Second)
	if err := b.allow(now); err != nil {
		t.Fatalf("half-open allow: %v", err)
	}
	b.recordFailure(now) // trip #2: cooldown doubled to 20s
	if got := retryAfter(t, b, now); got != 20*time.Second {
		t.Fatalf("second retryAfter = %s, want 20s", got)
	}
	b.recordSuccess()
	if b.open(now) {
		t.Fatal("should be closed after success")
	}
}

func TestCircuitBreakerCooldownCapped(t *testing.T) {
	b := newCircuitBreaker(1, 10*time.Second, 15*time.Second)
	now := time.Unix(1000, 0)
	b.recordFailure(now) // trip #1: 10s -> clamped to 15s max
	now = now.Add(15*time.Second + time.Second)
	if err := b.allow(now); err != nil {
		t.Fatalf("half-open allow: %v", err)
	}
	b.recordFailure(now) // trip #2: stays capped at 15s, not 30s
	if got := retryAfter(t, b, now); got != 15*time.Second {
		t.Fatalf("cooldown not capped: retryAfter = %s, want 15s", got)
	}
}

// TestCircuitBreakerStragglerDoesNotEscalate verifies a failure that
// arrives after the breaker has already opened (a call admitted before
// the trip) is ignored rather than re-escalating the backoff.
func TestCircuitBreakerStragglerDoesNotEscalate(t *testing.T) {
	b := newCircuitBreaker(2, 10*time.Second, time.Minute)
	now := time.Unix(1000, 0)
	b.recordFailure(now)
	b.recordFailure(now)                  // open; cooldown 10s -> 20s
	b.recordFailure(now.Add(time.Second)) // straggler: ignored
	after := now.Add(20*time.Second + time.Second)
	if err := b.allow(after); err != nil {
		t.Fatalf("half-open allow: %v", err)
	}
	b.recordFailure(after)
	if got := retryAfter(t, b, after); got != 20*time.Second {
		t.Errorf("straggler escalated cooldown: retryAfter = %s, want 20s", got)
	}
}

func TestControllerUnhealthy(t *testing.T) {
	for _, code := range []int{http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout} {
		if !controllerUnhealthy(code) {
			t.Errorf("controllerUnhealthy(%d) = false, want true", code)
		}
	}
	for _, code := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusNotFound, http.StatusConflict, http.StatusGone, http.StatusNotImplemented} {
		if controllerUnhealthy(code) {
			t.Errorf("controllerUnhealthy(%d) = true, want false", code)
		}
	}
}

func TestNewClientTimeout(t *testing.T) {
	c := NewClient("http://127.0.0.1:8889", "")
	if c.http.Timeout != defaultTimeout {
		t.Errorf("NewClient timeout = %s, want %s", c.http.Timeout, defaultTimeout)
	}
	c2 := NewClientWithTimeout("http://127.0.0.1:8889", "", 5*time.Second)
	if c2.http.Timeout != 5*time.Second {
		t.Errorf("NewClientWithTimeout timeout = %s, want 5s", c2.http.Timeout)
	}
}

func TestClientTripsBreakerOnServerErrors(t *testing.T) {
	c, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	for i := 0; i < breakerThreshold; i++ {
		if _, err := c.ListSnapshots(context.Background()); err == nil {
			t.Fatalf("call %d: expected 503 error", i+1)
		}
	}
	_, err := c.ListSnapshots(context.Background())
	if _, ok := err.(*CircuitOpenError); !ok {
		t.Fatalf("expected *CircuitOpenError, got %T: %v", err, err)
	}
}

// TestClientDoesNotTripOnClientErrors verifies 4xx responses (a
// responsive controller giving a definitive answer) never trip the
// breaker.
func TestClientDoesNotTripOnClientErrors(t *testing.T) {
	c, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	for i := 0; i < breakerThreshold*3; i++ {
		_, err := c.ListSnapshots(context.Background())
		if err == nil {
			t.Fatalf("call %d: expected 404 error", i+1)
		}
		if _, ok := err.(*CircuitOpenError); ok {
			t.Fatalf("call %d: breaker tripped on a 404", i+1)
		}
	}
}

// TestClientDoesNotTripWithinOneCall verifies the breaker counts one
// failure per do() call, not per retry attempt: a single call that
// exhausts its internal network-error retries must not trip the breaker.
func TestClientDoesNotTripWithinOneCall(t *testing.T) {
	c := NewClient("http://127.0.0.1:1", "") // closed port: refused fast
	_, err := c.ListSnapshots(context.Background())
	if err == nil {
		t.Fatal("expected connection error")
	}
	if _, ok := err.(*CircuitOpenError); ok {
		t.Fatal("breaker tripped within one call's retry loop (per-attempt over-counting)")
	}
}

// retryAfter extracts CircuitOpenError.RetryAfter from allow().
func retryAfter(t *testing.T, b *circuitBreaker, now time.Time) time.Duration {
	t.Helper()
	err := b.allow(now)
	ce, ok := err.(*CircuitOpenError)
	if !ok {
		t.Fatalf("allow error = %v, want *CircuitOpenError", err)
	}
	return ce.RetryAfter
}

// TestIsTimeout verifies timeout detection used to decide whether a
// network error is retried.
func TestIsTimeout(t *testing.T) {
	if !isTimeout(context.DeadlineExceeded) {
		t.Error("context.DeadlineExceeded should be a timeout")
	}
	if isTimeout(errors.New("connection refused")) {
		t.Error("plain error should not be a timeout")
	}
}
