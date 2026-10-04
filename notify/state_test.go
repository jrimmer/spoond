package notify

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestStateFileRecordAndLoad: a dropped delivery lands in the state
// file, redacted (the caller redacts, but the file only ever carries
// what it was given), and LoadFailures returns it within the horizon.
func TestStateFileRecordAndLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state.json")
	sf := &stateFile{path: path}
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	sf.record(Failure{At: now, Webhook: 1, Error: "HTTP 500"}, now)

	fails, err := LoadFailures(path, now)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(fails) != 1 || fails[0].Webhook != 1 || fails[0].Error != "HTTP 500" {
		t.Fatalf("fails = %+v", fails)
	}

	// A failure older than 24 h is pruned on the next record.
	sf.record(Failure{At: now.Add(-48 * time.Hour), Webhook: 0, Error: "old"}, now)
	sf.record(Failure{At: now.Add(time.Minute), Webhook: 2, Error: "new"}, now.Add(time.Minute))
	fails, err = LoadFailures(path, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("load 2: %v", err)
	}
	if len(fails) != 2 {
		t.Fatalf("horizon prune kept %d: %+v", len(fails), fails)
	}
	if fails[0].Webhook != 1 || fails[1].Webhook != 2 {
		t.Fatalf("order = %+v", fails)
	}
}

// TestStateFileEmptyPathIsNoop: state in memory only (no StatePath)
// never touches the disk.
func TestStateFileEmptyPathIsNoop(t *testing.T) {
	var sf *stateFile
	sf.record(Failure{At: time.Now(), Webhook: 0, Error: "x"}, time.Now()) // nil receiver: no panic
	(&stateFile{}).record(Failure{At: time.Now(), Webhook: 0, Error: "x"}, time.Now())
}

// TestStateFileSurvivesCorruption: a corrupt state file is replaced,
// not propagated — delivery must not be disturbed by doctor state.
func TestStateFileSurvivesCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	sf := &stateFile{path: path}
	now := time.Now()
	sf.record(Failure{At: now, Webhook: 0, Error: "e"}, now)
	fails, err := LoadFailures(path, now)
	if err != nil || len(fails) != 1 {
		t.Fatalf("after corruption = %+v, %v", fails, err)
	}
}

// TestLoadFailuresPrunesToHorizon: entries past 24 h never come back
// from a file, even fresh ones written by an older process.
func TestLoadFailuresPrunesToHorizon(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	b, _ := json.Marshal([]Failure{
		{At: now.Add(-23 * time.Hour), Webhook: 0, Error: "kept"},
		{At: now.Add(-25 * time.Hour), Webhook: 1, Error: "pruned"},
	})
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	fails, err := LoadFailures(path, now)
	if err != nil || len(fails) != 1 || fails[0].Webhook != 0 {
		t.Fatalf("fails = %+v, %v", fails, err)
	}
}

// TestPruneFailuresCap: more than maxFailures entries keep the newest.
func TestPruneFailuresCap(t *testing.T) {
	now := time.Now()
	var fails []Failure
	for i := 0; i < maxFailures+10; i++ {
		fails = append(fails, Failure{At: now.Add(time.Duration(i) * time.Second), Webhook: i})
	}
	out := pruneFailures(fails, now)
	if len(out) != maxFailures {
		t.Fatalf("kept %d, want %d", len(out), maxFailures)
	}
	if out[0].Webhook != 10 || out[len(out)-1].Webhook != maxFailures+9 {
		t.Fatalf("kept the wrong end: first=%d last=%d", out[0].Webhook, out[len(out)-1].Webhook)
	}
}

// TestTestHook lets tests drive a check pass by hand: a registered
// check's events land in the queue synchronously.
func TestTestHook(t *testing.T) {
	clock := &stepClock{now: checkNow}
	n := New(Config{Now: clock.Now})
	fired := false
	n.AddCheck(func(_ context.Context, _ time.Time) []Event {
		fired = true
		return []Event{{Key: "unit.inactive", Severity: Critical, Title: "down", At: clock.Now()}}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n.TestHook().RunChecksNow(ctx)
	if !fired {
		t.Fatal("check did not run")
	}
	if n.TestHook().QueueLen() != 1 {
		t.Fatalf("queue = %d, want 1", n.TestHook().QueueLen())
	}
}

// TestCheckPassResolvedOnlyAfterAlert: a healthy pass emits resolved
// events for every key it watches, but only keys an alert actually
// opened are enqueued — a healthy system must stay silent instead of
// resolving conditions nobody was told about (and eating the webhooks'
// rate limits with the resolutions).
func TestCheckPassResolvedOnlyAfterAlert(t *testing.T) {
	clock := &stepClock{now: checkNow}
	n := New(Config{Now: clock.Now})
	// A check that mirrors diskCheck's shape: a resolved event for both
	// of its keys every pass, alerting only when its input says so.
	healthy := true
	n.AddCheck(func(_ context.Context, _ time.Time) []Event {
		if healthy {
			return []Event{resolved("disk.warn", Warn, clock.Now()), resolved("disk.danger", Critical, clock.Now())}
		}
		return []Event{{Key: "disk.warn", Severity: Warn, Title: "past warn", At: clock.Now()}}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Healthy pass: nothing is enqueued at all.
	n.TestHook().RunChecksNow(ctx)
	if got := n.TestHook().QueueLen(); got != 0 {
		t.Fatalf("healthy pass enqueued %d events, want 0", got)
	}

	// Alert pass: the alert goes out; the resolved events the check
	// does not emit this pass are beside the point.
	healthy = false
	n.TestHook().RunChecksNow(ctx)
	if got := n.TestHook().QueueLen(); got != 1 {
		t.Fatalf("alert pass enqueued %d events, want 1", got)
	}
	ev := <-n.queue
	if ev.Key != "disk.warn" || ev.Resolved {
		t.Fatalf("alert pass enqueued %+v", ev)
	}

	// Healthy again: the previously alerted key resolves once. The
	// never-alerted danger key stays silent.
	healthy = true
	n.TestHook().RunChecksNow(ctx)
	if got := n.TestHook().QueueLen(); got != 1 {
		t.Fatalf("clear pass enqueued %d events, want 1", got)
	}
	ev = <-n.queue
	if ev.Key != "disk.warn" || !ev.Resolved {
		t.Fatalf("clear pass enqueued %+v", ev)
	}

	// And the pass after that is silent again — one resolution per
	// clear, not one every minute.
	n.TestHook().RunChecksNow(ctx)
	if got := n.TestHook().QueueLen(); got != 0 {
		t.Fatalf("second clear pass enqueued %d events, want 0", got)
	}
}

// TestCheckEventStampsResolveSeverity: a resolved event without its
// own severity carries the level the alert went out at, so a
// min_severity warn webhook that heard about the problem also hears
// the clear.
func TestCheckEventStampsResolveSeverity(t *testing.T) {
	n := New(Config{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n.AddCheck(func(context.Context, time.Time) []Event {
		return []Event{{Key: "backup.stale", Severity: Warn, Title: "stale", At: checkNow}}
	})
	n.TestHook().RunChecksNow(ctx)
	ev := <-n.queue // the alert opens the key
	if ev.Resolved {
		t.Fatalf("expected the alert, got %+v", ev)
	}
	n.AddCheck(func(context.Context, time.Time) []Event {
		return []Event{resolved("backup.stale", "", checkNow)} // severity forgotten
	})
	n.TestHook().RunChecksNow(ctx)
	ev = <-n.queue // first check's repeat alert
	if ev.Resolved {
		t.Fatalf("expected the repeat alert, got %+v", ev)
	}
	ev = <-n.queue // the resolution, stamped with the alert's severity
	if !ev.Resolved || ev.Severity != Warn {
		t.Fatalf("resolved event = %+v, want the alert's warn severity", ev)
	}
}

// TestRunChecksHugepagesProbe: a hugepage probe installed after New is
// included in the pass.
func TestRunChecksHugepagesProbe(t *testing.T) {
	clock := &stepClock{now: checkNow}
	n := New(Config{Now: clock.Now})
	n.SetHugepages(func() (uint64, uint64, uint64, uint64, error) {
		return 1000, 950, 0, 1, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n.TestHook().RunChecksNow(ctx)
	evs := make([]Event, 0, n.TestHook().QueueLen())
	for len(evs) < n.TestHook().QueueLen() {
		evs = append(evs, <-n.queue)
	}
	if _, ok := findByKey(t, evs, KeyHugepagesDanger); !ok {
		t.Fatalf("hugepage check did not fire: %+v", evs)
	}
}

// TestRunChecksRecovers: a panicking check is skipped, its successors
// still run.
func TestRunChecksRecovers(t *testing.T) {
	clock := &stepClock{now: checkNow}
	n := New(Config{Now: clock.Now})
	n.AddCheck(func(context.Context, time.Time) []Event { panic("boom") })
	n.AddCheck(func(_ context.Context, _ time.Time) []Event {
		return []Event{{Key: "after.panic", Severity: Info, Title: "t", At: clock.Now()}}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n.TestHook().RunChecksNow(ctx)
	if n.TestHook().QueueLen() != 1 {
		t.Fatalf("queue = %d, want the surviving check's 1 event", n.TestHook().QueueLen())
	}
}
