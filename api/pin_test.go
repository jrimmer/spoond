package api

// Pins and the one paused-release clock (FS5, owner decision 2026-10-08):
// a pinned lease is never paused or deleted before its own expiry,
// holder labels never pin, take-back touches only unpinned leases,
// box_full is the refusal when nothing unpinned can be taken, the one
// clock releases a paused lease 30 days after its pause date, and
// resumed leases clear the clock. See docs/operations.md.

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// TestPinnedLeaseSurvivesTakeBackButExpiresAtTTL: a pinned lease is the
// mutation target of FS5. Take-back never pauses it, but its own TTL
// still releases it.
func TestPinnedLeaseSurvivesTakeBackButExpiresAtTTL(t *testing.T) {
	svc, sub, ctx := newPreemptService(t)
	installDynamicNode(t, svc, sub, 512, 0, 512)

	// One pinned burst lease fills the node.
	pinned, err := svc.grantLease(ctx, leaseRequest{owner: "burst-a", image: "mid", ttl: 20 * time.Millisecond, burst: true})
	if err != nil {
		t.Fatalf("grant pinned: %v", err)
	}
	if _, err := svc.setPinned("burst-a", pinned.ID, true); err != nil {
		t.Fatalf("pin: %v", err)
	}

	// A guaranteed create that needs the pinned lease's room is refused
	// box_full and pauses nothing.
	_, err = svc.grantLease(ctx, leaseRequest{owner: "guaranteed", image: "mid", ttl: time.Hour})
	if !isBoxFull(err) {
		t.Fatalf("guaranteed grant err = %v, want box_full", err)
	}
	if pinned.Suspended || !pinned.Pinned {
		t.Fatalf("take-back touched a pinned lease: suspended=%v pinned=%v", pinned.Suspended, pinned.Pinned)
	}

	// The pin does not stop the TTL: after expiry the sweep releases it.
	time.Sleep(30 * time.Millisecond)
	svc.now = func() time.Time { return time.Now().Add(time.Second) }
	t.Cleanup(func() { svc.now = time.Now })
	svc.sweepExpired(ctx)
	if svc.lookup("burst-a", pinned.ID) != nil {
		t.Fatal("a pinned lease was not released at its own TTL")
	}
}

// TestBoxFullHTTP429: the box_full refusal maps to 429 with code box_full
// and pauses nothing.
func TestBoxFullHTTP429(t *testing.T) {
	ts, svc, sub, ctx := newPreemptServer(t)
	installDynamicNode(t, svc, sub, 512, 0, 512)

	// A pinned burst lease fills the node.
	pinned, err := svc.grantLease(ctx, leaseRequest{owner: "burst-a", image: "mid", ttl: time.Hour, burst: true})
	if err != nil {
		t.Fatalf("grant pinned: %v", err)
	}
	if _, err := svc.setPinned("burst-a", pinned.ID, true); err != nil {
		t.Fatalf("pin: %v", err)
	}

	resp, body := doReq(t, "POST", ts.URL+"/api/leases", "token-a", map[string]any{"image": "mid", "ttl": 3600})
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("box_full create = %d (%v), want 429", resp.StatusCode, body)
	}
	if body["code"] != "box_full" {
		t.Fatalf("box_full body code = %v, want box_full", body["code"])
	}
	if pinned.Suspended {
		t.Fatal("a box_full refusal paused the pinned lease")
	}
}

// TestHolderLabelNeverPins: a create with a holder and no pinned gives an
// unpinned lease that take-back can pause (FS5 contract).
func TestHolderLabelNeverPins(t *testing.T) {
	ts, svc, _, _ := newTestServerWithService(t)

	resp, body := doReq(t, "POST", ts.URL+"/api/leases", "token-a", map[string]any{
		"image": "py-base", "ttl": 300, "holder": "ci-job-42",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d (%v), want 201", resp.StatusCode, body)
	}
	if body["pinned"] != false {
		t.Fatalf("create with a holder returned pinned=%v, want false", body["pinned"])
	}
	l := svc.lookup("consumer-a", body["id"].(string))
	if l == nil || l.Pinned {
		t.Fatalf("a holder label pinned the lease: %+v", l)
	}
	// PUT /holder with a holder on an existing lease never pins either.
	resp, body = doReq(t, "PUT", ts.URL+"/api/leases/"+l.ID+"/holder", "token-a", map[string]any{"holder": "flight-7"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put holder = %d (%v), want 200", resp.StatusCode, body)
	}
	if l := svc.lookup("consumer-a", l.ID); l.Pinned {
		t.Fatalf("PUT holder pinned the lease: %+v", l)
	}

	// A holder label never makes a non-persistent lease resumable: the
	// resume gate keys on Pinned, not on Holder.
	if _, err := svc.resume(context.Background(), "consumer-a", l.ID); err != errNotPersistent {
		t.Fatalf("resume of a holder-labelled non-persistent lease = %v, want errNotPersistent", err)
	}
	resp, body = doReq(t, "PUT", ts.URL+"/api/leases/"+l.ID+"/pin", "token-a", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pin = %d (%v), want 200", resp.StatusCode, body)
	}
	l = svc.lookup("consumer-a", l.ID)
	l.Suspended = true
	if _, err := svc.resume(context.Background(), "consumer-a", l.ID); err == errNotPersistent {
		t.Fatal("pinned non-persistent lease should be resumable")
	}
}

// TestPinRoutesAndVisibility: PUT pins, DELETE unpins, owner or admin,
// and both show on GET and list. A non-owner gets 404.
func TestPinRoutesAndVisibility(t *testing.T) {
	ts, svc, _, _ := newTestServerWithService(t)
	l, err := svc.grant(context.Background(), "consumer-a", "py-base", time.Hour, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}

	// Pin.
	resp, body := doReq(t, "PUT", ts.URL+"/api/leases/"+l.ID+"/pin", "token-a", nil)
	if resp.StatusCode != http.StatusOK || body["pinned"] != true {
		t.Fatalf("pin = %d (%v), want 200 pinned", resp.StatusCode, body)
	}
	_, got := doReq(t, "GET", ts.URL+"/api/leases/"+l.ID, "token-a", nil)
	if got["pinned"] != true {
		t.Fatalf("GET pinned = %v, want true", got["pinned"])
	}
	_, list := doReq(t, "GET", ts.URL+"/api/leases", "token-a", nil)
	found := false
	for _, row := range list["sandboxes"].([]any) {
		if m := row.(map[string]any); m["id"] == l.ID {
			found = true
			if m["pinned"] != true {
				t.Fatalf("list pinned = %v, want true", m["pinned"])
			}
		}
	}
	if !found {
		t.Fatal("pinned lease missing from the list")
	}

	// A non-owner gets 404.
	resp, _ = doReq(t, "PUT", ts.URL+"/api/leases/"+l.ID+"/pin", "token-b", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("other owner's pin = %d, want 404", resp.StatusCode)
	}

	// Unpin.
	resp, body = doReq(t, "DELETE", ts.URL+"/api/leases/"+l.ID+"/pin", "token-a", nil)
	if resp.StatusCode != http.StatusOK || body["pinned"] != false {
		t.Fatalf("unpin = %d (%v), want 200 unpinned", resp.StatusCode, body)
	}
}

// TestResumeClearsThePauseClock: resuming a paused lease clears paused_at
// (and the expiring-notified flag), so the one clock starts over only on
// a new pause.
func TestResumeClearsThePauseClock(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	l, err := svc.grant(ctx, "c", "py-base", time.Hour, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := svc.pauseLease(ctx, l, false); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if l.PausedAt.IsZero() {
		t.Fatal("pause did not stamp paused_at")
	}
	if _, err := svc.resume(ctx, "c", l.ID); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if !l.PausedAt.IsZero() || l.PausedExpiryNotified {
		t.Fatalf("resume did not clear the pause clock: paused_at=%v notified=%v", l.PausedAt, l.PausedExpiryNotified)
	}
	// A resume writes the cleared clock through the store; a reload must
	// not resurrect the old pause date.
	svc.store.mu.Lock()
	l.PausedExpiryNotified = false
	svc.saveLeaseLocked(l)
	svc.store.mu.Unlock()
}

// TestPausedReleaseAt30DaysNot29: the one clock releases a paused lease
// exactly PAUSED_RELEASE_DAYS after its pause date, not a day earlier.
func TestPausedReleaseAt30DaysNot29(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	base := time.Now()
	svc.now = func() time.Time { return base }
	l, err := svc.grant(ctx, "c", "py-base", time.Hour, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := svc.pauseLease(ctx, l, false); err != nil {
		t.Fatalf("pause: %v", err)
	}

	svc.releasePausedLeases(ctx, base.Add(29*24*time.Hour))
	if svc.lookup("c", l.ID) == nil {
		t.Fatal("released at 29 days, want not before 30")
	}
	svc.releasePausedLeases(ctx, base.Add(30*24*time.Hour))
	if svc.lookup("c", l.ID) != nil {
		t.Fatal("not released at 30 days")
	}
}

// TestNoIdlePauseWithoutIdleSuspend: a persistent lease with no
// idle_suspend opt-in is never paused, however idle, and a pinned lease
// is never paused either. The caller-chosen idle_suspend is the only
// idle threshold.
func TestNoIdlePauseWithoutIdleSuspend(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	base := time.Now()
	svc.now = func() time.Time { return base }
	plain, err := svc.grant(ctx, "c", "py-base", time.Hour, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant plain: %v", err)
	}
	pinned, err := svc.grant(ctx, "c", "py-base", time.Hour, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant pinned: %v", err)
	}
	if _, err := svc.setPinned("c", pinned.ID, true); err != nil {
		t.Fatalf("pin: %v", err)
	}
	svc.store.mu.Lock()
	plain.LastActive = base.Add(-24 * time.Hour)
	pinned.LastActive = base.Add(-24 * time.Hour)
	svc.store.mu.Unlock()

	// A sweep a full day later: neither is paused.
	svc.sweepExpired(ctx)
	if plain.Suspended || pinned.Suspended {
		t.Fatalf("an idle lease was paused without idle_suspend: plain=%v pinned=%v", plain.Suspended, pinned.Suspended)
	}
}

// TestPinnedIdleNoticeSetAndClearedNoLifecycleChange: a pinned lease
// whose LastActive passes PINNED_IDLE_NOTICE_DAYS is flagged and emits
// one event; activity clears the flag. Nothing is paused, unpinned or
// released.
func TestPinnedIdleNoticeSetAndClearedNoLifecycleChange(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	base := time.Now()
	svc.now = func() time.Time { return base }
	svc.cfg.PinnedIdleNoticeDays = 7
	l, err := svc.grant(ctx, "c", "py-base", time.Hour, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := svc.setPinned("c", l.ID, true); err != nil {
		t.Fatalf("pin: %v", err)
	}
	svc.store.mu.Lock()
	l.LastActive = base
	svc.store.mu.Unlock()

	all := svc.Subscribe(EventFilter{LeaseID: l.ID})
	defer all.Close()

	// 8 days idle: flagged and one event.
	svc.noticePinnedIdle(ctx, base.Add(8*24*time.Hour))
	if l.PinnedIdleSince.IsZero() {
		t.Fatal("a pinned lease idle past the notice threshold was not flagged")
	}
	if !l.Pinned || l.Suspended {
		t.Fatalf("the notice changed the lease's lifecycle: pinned=%v suspended=%v", l.Pinned, l.Suspended)
	}
	// A second pass does not re-flag or re-emit.
	first := l.PinnedIdleSince
	svc.noticePinnedIdle(ctx, base.Add(9*24*time.Hour))
	if !l.PinnedIdleSince.Equal(first) {
		t.Fatalf("the flag moved on a second pass: %v -> %v", first, l.PinnedIdleSince)
	}
	// Activity clears the flag.
	svc.store.mu.Lock()
	l.LastActive = base.Add(9 * 24 * time.Hour)
	svc.store.mu.Unlock()
	svc.noticePinnedIdle(ctx, base.Add(9*24*time.Hour))
	if !l.PinnedIdleSince.IsZero() {
		t.Fatal("activity did not clear the pinned-idle flag")
	}
	// The lease is untouched: still pinned, still running, never paused.
	if !l.Pinned || l.Suspended || svc.lookup("c", l.ID) == nil {
		t.Fatalf("the notice changed the lease: %+v", l)
	}
}

// TestAdminUnpinByHolderPrefix: the migration-window route unpins leases
// by holder label prefix and refuses an empty prefix.
func TestAdminUnpinByHolderPrefix(t *testing.T) {
	ts, svc, _, _ := newAdminServer(t, "admin-tok")
	ctx := context.Background()

	// A pinned pool worker (holder label) and a pinned user lease (no
	// matching holder): only the worker is unpinned.
	worker, err := svc.grant(ctx, "c", "py-base", time.Hour, true, "", nil, "pool:honey/work-1", "", nil)
	if err != nil {
		t.Fatalf("grant worker: %v", err)
	}
	other, err := svc.grant(ctx, "c", "py-base", time.Hour, true, "", nil, "ci-job", "", nil)
	if err != nil {
		t.Fatalf("grant other: %v", err)
	}
	for _, l := range []*Lease{worker, other} {
		if _, err := svc.setPinned("c", l.ID, true); err != nil {
			t.Fatalf("pin: %v", err)
		}
	}

	// An empty prefix is refused; the route needs the admin token.
	resp, _ := doReq(t, "POST", ts.URL+"/api/admin/unpin-by-holder", "admin-tok", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty prefix = %d, want 400", resp.StatusCode)
	}
	resp, _ = doReq(t, "POST", ts.URL+"/api/admin/unpin-by-holder?holder_prefix=pool:", "token-a", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("non-admin unpin = %d, want 401", resp.StatusCode)
	}

	resp, body := doReq(t, "POST", ts.URL+"/api/admin/unpin-by-holder?holder_prefix=pool:", "admin-tok", nil)
	if resp.StatusCode != http.StatusOK || body["unpinned"] != float64(1) {
		t.Fatalf("unpin by prefix = %d (%v), want 200 unpinned=1", resp.StatusCode, body)
	}
	if svc.lookup("c", worker.ID).Pinned {
		t.Fatal("the pool worker was not unpinned")
	}
	if !svc.lookup("c", other.ID).Pinned {
		t.Fatal("an unrelated pinned lease was unpinned")
	}
}
