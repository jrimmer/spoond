package api

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v2/notify"
)

// sink records what the service forwards to the notifier. Mutex-
// guarded: the notify loop appends from its own goroutine while the
// test reads.
type sink struct {
	mu     sync.Mutex
	events []struct {
		key      string
		severity string
		resolved bool
	}
}

func (s *sink) Enqueue(ev notify.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, struct {
		key      string
		severity string
		resolved bool
	}{ev.Key, string(ev.Severity), ev.Resolved})
}

func (s *sink) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.events)
}

func (s *sink) at(i int) (key, severity string, resolved bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.events[i]
	return e.key, e.severity, e.resolved
}

// TestNotifyLoopForwardsLostAndHeldActions: the lease event bus's lost
// and held_action events reach the notifier; lifecycle churn does not.
func TestNotifyLoopForwardsLostAndHeldActions(t *testing.T) {
	svc, _, _ := newTestService(t)
	rec := &sink{}
	svc.SetNotifier(rec)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		svc.runNotifyLoop(ctx)
		close(done)
	}()
	// Let the loop subscribe before emitting; the bus delivers only to
	// registered subscribers.
	time.Sleep(50 * time.Millisecond)

	svc.emitLeaseEvent("lease-1", "owner-a", LeaseLost, "no checkpoint to recover from")
	svc.emitLeaseEvent("lease-2", "owner-b", LeaseHeldAction, "idle/suspend_idle: idle 4h0m0s")
	svc.emitLeaseEvent("lease-3", "owner-b", LeaseHeldAction, "stale/release: suspended 7d")
	svc.emitLeaseEvent("lease-4", "owner-a", LeaseCreated, "granted") // churn: dropped
	svc.emitLeaseEvent("lease-5", "owner-a", LeaseReleased, "gone")   // churn: dropped

	deadline := time.Now().Add(2 * time.Second)
	for rec.len() < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done

	if rec.len() != 3 {
		t.Fatalf("forwarded %d events, want 3", rec.len())
	}
	key0, sev0, _ := rec.at(0)
	if key0 != "lease.lost.lease-1" || sev0 != "critical" {
		t.Fatalf("lost = %q/%q", key0, sev0)
	}
	// suspend_idle is warn; release is critical.
	key1, sev1, _ := rec.at(1)
	if key1 != "held.idle.lease-2" || sev1 != "warn" {
		t.Fatalf("held suspend = %q/%q", key1, sev1)
	}
	key2, sev2, _ := rec.at(2)
	if key2 != "held.stale.lease-3" || sev2 != "critical" {
		t.Fatalf("held release = %q/%q", key2, sev2)
	}
}

// TestNotifyLoopNilNotifier: without SetNotifier the loop is a no-op
// (and Start must not spawn it).
func TestNotifyLoopNilNotifier(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	svc.runNotifyLoop(ctx) // returns at once
}

// TestNotifyLoopStopsOnCancel: the loop exits when ctx ends.
func TestNotifyLoopStopsOnCancel(t *testing.T) {
	svc, _, _ := newTestService(t)
	svc.SetNotifier(&sink{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		svc.runNotifyLoop(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("notify loop did not stop on cancel")
	}
}

// TestParseHeldDetail pins the key parts of a held_action detail.
func TestParseHeldDetail(t *testing.T) {
	for detail, want := range map[string][2]string{
		"idle/suspend_idle: idle 4h0m0s": {"idle", "suspend_idle"},
		"stale/release: suspended 168h":  {"stale", "release"},
		"expiry/expire: hold lapsed":     {"expiry", "expire"},
		"pressure/release: disk 4%":      {"pressure", "release"},
		"critical/release: disk 3%":      {"critical", "release"},
		"garbage-without-slash":          {"held", "action"},
		"":                               {"held", "action"},
	} {
		rule, action := parseHeldDetail(detail)
		if rule != want[0] || action != want[1] {
			t.Fatalf("parseHeldDetail(%q) = %q/%q, want %q/%q", detail, rule, action, want[0], want[1])
		}
	}
}

// TestShortID is rune-safe and shortens with the ellipsis.
func TestShortID(t *testing.T) {
	id := "0123456789abcdef"
	if got := shortID(id); got != "0123456789ab…" {
		t.Fatalf("shortID = %q", got)
	}
	if got := shortID("short"); got != "short" {
		t.Fatalf("shortID(short) = %q", got)
	}
	// A multi-byte rune straddling the 12-byte cut is not garbled.
	multi := "あいうえおかきくけこさし"
	got := shortID(multi)
	for _, r := range got {
		if r == '�' {
			t.Fatalf("shortID split a rune: %q", got)
		}
	}
	if len(got) > 12+3 {
		t.Fatalf("shortID too long: %q", got)
	}
}

// TestGCTracker: nil before the first pass, the error after a failing
// pass, nil again after a good one.
func TestGCTracker(t *testing.T) {
	g := newGCTracker()
	if err := g.Last(); err != nil {
		t.Fatalf("fresh tracker = %v, want nil", err)
	}
	boom := errors.New("gc: disk full")
	g.Set(boom)
	if err := g.Last(); !errors.Is(err, boom) {
		t.Fatalf("after failed pass = %v", err)
	}
	g.Set(nil)
	if err := g.Last(); err != nil {
		t.Fatalf("after good pass = %v, want nil", err)
	}
}

// TestServiceGCLastError: before any pass the check source is nil (an
// unrun GC is not a failure), and recordGCOutcome feeds it.
func TestServiceGCLastError(t *testing.T) {
	svc, _, _ := newTestService(t)
	last := svc.GCLastError()
	if err := last(); err != nil {
		t.Fatalf("before any pass = %v", err)
	}
	boom := errors.New("gc: list builds: db locked")
	svc.recordGCOutcome(boom)
	if err := last(); !errors.Is(err, boom) {
		t.Fatalf("after failed pass = %v", err)
	}
	svc.recordGCOutcome(nil)
	if err := last(); err != nil {
		t.Fatalf("after good pass = %v", err)
	}
}

// TestRecordGCSkippedIsNotFailure: gcOnce on a drained service records
// nothing — a pass that did not run is not a failure.
func TestRecordGCSkippedIsNotFailure(t *testing.T) {
	svc, _, _ := newTestService(t)
	svc.draining.Store(true)
	boom := errors.New("sentinel: should stay invisible")
	svc.gcErr.Set(boom) // pre-seed to prove gcOnce does NOT overwrite
	defer svc.draining.Store(false)
	if err := svc.gcOnce(context.Background()); err != nil {
		t.Fatalf("drained gcOnce = %v", err)
	}
	if err := svc.gcErr.Last(); !errors.Is(err, boom) {
		t.Fatalf("drained pass overwrote the tracker: %v", err)
	}
}
