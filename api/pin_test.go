package api

// Pins and the one paused-release clock (FS5, owner decision 2026-10-08):
// a pinned lease is never paused or deleted before its own expiry,
// holder labels never pin, take-back touches only unpinned leases,
// box_full is the refusal when nothing unpinned can be taken, the one
// clock releases a paused lease 30 days after its pause date, and
// resumed leases clear the clock. See docs/operations.md.

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v2/substrate"
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

	// A suspended non-persistent lease the caller owns can be explicitly
	// resumed (v3.0, spoond-k0uz M5): persistence is not a resume gate,
	// and a holder label does not change that either way. Pause it for
	// real so it has a resume build, like resume-on-use requires.
	if _, err := svc.pauseLease(context.Background(), l, false); err != nil {
		t.Fatalf("pause non-persistent lease: %v", err)
	}
	if _, err := svc.resume(context.Background(), "consumer-a", l.ID); err != nil {
		t.Fatalf("resume of a suspended non-persistent lease = %v, want success", err)
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

// TestCreatePinnedTrueHTTP: a create with "pinned": true pins the new
// lease at the HTTP level (spoond-k0uz H1: the field was decoded but
// dropped by the request path; the other tests call grantLease
// directly).
func TestCreatePinnedTrueHTTP(t *testing.T) {
	ts, svc, _, _ := newTestServerWithService(t)

	resp, body := doReq(t, "POST", ts.URL+"/api/leases", "token-a", map[string]any{
		"image": "py-base", "ttl": 300, "pinned": true,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create pinned = %d (%v), want 201", resp.StatusCode, body)
	}
	if body["pinned"] != true {
		t.Fatalf("create response pinned = %v, want true", body["pinned"])
	}
	l := svc.lookup("consumer-a", body["id"].(string))
	if l == nil || !l.Pinned {
		t.Fatalf("the HTTP create did not pin the lease: %+v", l)
	}

	// A create with the field absent (or false) stays unpinned.
	resp, body = doReq(t, "POST", ts.URL+"/api/leases", "token-a", map[string]any{
		"image": "py-base", "ttl": 300, "pinned": false,
	})
	if resp.StatusCode != http.StatusCreated || body["pinned"] != false {
		t.Fatalf("create unpinned = %d (%v), want 201 pinned=false", resp.StatusCode, body)
	}
}

// TestPinnedNotIdleSuspended: a pinned lease with its own idle_suspend
// is never paused by the idle sweep (spoond-k0uz H2, owner: "Pins don't
// pause"). A caller's explicit POST /pause still works.
func TestPinnedNotIdleSuspended(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	base := time.Now()
	svc.now = func() time.Time { return base }
	pinned, err := svc.grant(ctx, "c", "py-base", time.Hour, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := svc.setPinned("c", pinned.ID, true); err != nil {
		t.Fatalf("pin: %v", err)
	}
	// The lease's own idle_suspend opt-in would fire, but the pin wins.
	if _, err := svc.setIdlePolicy(pinned, 60); err != nil {
		t.Fatalf("setIdlePolicy: %v", err)
	}
	svc.store.mu.Lock()
	pinned.LastActive = base.Add(-time.Hour)
	svc.store.mu.Unlock()

	svc.suspendIdleLeases(ctx, base)
	if pinned.Suspended {
		t.Fatal("the idle sweep paused a pinned lease despite its idle_suspend")
	}
	// The owner's own explicit pause is their action and still works.
	if _, err := svc.pauseLease(ctx, pinned, false); err != nil {
		t.Fatalf("explicit pause of a pinned lease: %v", err)
	}
	if !pinned.Suspended {
		t.Fatal("an explicit pause did not suspend a pinned lease")
	}
}

// TestPinnedNotPausedByHostDefaultIdle: the same with the host default
// idle_suspend (idle_suspend absent): effectiveIdleSuspend must return 0
// for a pinned lease (spoond-k0uz H2).
func TestPinnedNotPausedByHostDefaultIdle(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	svc.cfg.IdleSuspendDefault = 60
	ctx := context.Background()

	base := time.Now()
	svc.now = func() time.Time { return base }
	pinned, err := svc.grant(ctx, "c", "py-base", time.Hour, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := svc.setPinned("c", pinned.ID, true); err != nil {
		t.Fatalf("pin: %v", err)
	}
	if got := svc.effectiveIdleSuspend(pinned); got != 0 {
		t.Fatalf("effectiveIdleSuspend(pinned) = %d, want 0", got)
	}
	// An unpinned twin under the same host default does suspend, so the
	// test proves the pin is what stops it.
	unpinned, err := svc.grant(ctx, "c", "py-base", time.Hour, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant unpinned: %v", err)
	}
	svc.store.mu.Lock()
	pinned.LastActive = base.Add(-time.Hour)
	unpinned.LastActive = base.Add(-time.Hour)
	svc.store.mu.Unlock()

	svc.suspendIdleLeases(ctx, base)
	if pinned.Suspended {
		t.Fatal("the idle sweep paused a pinned lease under the host default")
	}
	if !unpinned.Suspended {
		t.Fatal("the idle sweep did not pause the unpinned lease under the host default")
	}
}

// TestPausedPinnedReleasedAt30: a pinned lease that is paused (the
// owner's own POST /pause, or a failed drain resume) is on the one clock
// and released 30 d after its pause date. A running pinned lease is
// never touched (owner decision 2026-10-09).
func TestPausedPinnedReleasedAt30(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	base := time.Now()
	svc.now = func() time.Time { return base }
	pausedPinned, err := svc.grant(ctx, "c", "py-base", time.Hour, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant paused pinned: %v", err)
	}
	if _, err := svc.setPinned("c", pausedPinned.ID, true); err != nil {
		t.Fatalf("pin: %v", err)
	}
	if _, err := svc.pauseLease(ctx, pausedPinned, false); err != nil {
		t.Fatalf("pause pinned: %v", err)
	}
	runningPinned, err := svc.grant(ctx, "c", "py-base", time.Hour, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant running pinned: %v", err)
	}
	if _, err := svc.setPinned("c", runningPinned.ID, true); err != nil {
		t.Fatalf("pin running: %v", err)
	}

	// A day short of 30: the paused pinned lease is still there.
	svc.releasePausedLeases(ctx, base.Add(29*24*time.Hour))
	if svc.lookup("c", pausedPinned.ID) == nil {
		t.Fatal("a paused pinned lease was released before 30 d")
	}
	// At 30 d it is released; the running pinned lease is never touched.
	svc.releasePausedLeases(ctx, base.Add(30*24*time.Hour))
	if svc.lookup("c", pausedPinned.ID) != nil {
		t.Fatal("a paused pinned lease was not released at 30 d")
	}
	if svc.lookup("c", runningPinned.ID) == nil {
		t.Fatal("a running pinned lease was released by the pause clock")
	}
}

// TestDrainedResumeFailedPausedAtOneClock: a lease the admin drain left
// suspended and whose resume then failed (resume_failed) keeps the pause
// date its drain pause stamped, so it is on the one clock and released
// 30 d after that pause (owner decision 2026-10-09).
func TestDrainedResumeFailedPausedAtOneClock(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()
	svc.cfg.UndrainResumeRetries = 0

	leases := grantAndDrain(t, svc, 1)
	target := leases[0]
	targetSandbox := target.SandboxID
	pauseAt := target.PausedAt
	if pauseAt.IsZero() {
		t.Fatal("drain did not stamp paused_at")
	}

	sub.createFn = func(c context.Context, req substrate.CreateRequest) (substrate.Sandbox, error) {
		if req.Resume && req.SandboxID == targetSandbox {
			return substrate.Sandbox{}, errors.New("failed to init envd: syncing took too long")
		}
		return sub.Fake.Create(c, req)
	}
	t.Cleanup(func() { sub.createFn = nil })

	svc.undrain(ctx)
	if target.SuspendReason != suspendReasonResumeFailed {
		t.Fatalf("suspend_reason = %q, want resume_failed", target.SuspendReason)
	}
	if !target.PausedAt.Equal(pauseAt) {
		t.Fatalf("resume_failed moved paused_at from the drain pause: %v -> %v", pauseAt, target.PausedAt)
	}

	// A day short of 30 d it is still there; at 30 d the one clock
	// releases it.
	svc.releasePausedLeases(ctx, pauseAt.Add(29*24*time.Hour))
	if svc.lookup("c", target.ID) == nil {
		t.Fatal("a drained resume_failed lease was released before 30 d")
	}
	svc.releasePausedLeases(ctx, pauseAt.Add(30*24*time.Hour))
	if svc.lookup("c", target.ID) != nil {
		t.Fatal("a drained resume_failed lease was not released at 30 d")
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

	// The aggregate admin_unpin event carries the lease id "-" placeholder:
	// it describes no single lease, and the SSE JSON's lease_id stays
	// well-formed while a per-lease filter never matches it (spoond-k0uz L10).
	events := svc.Subscribe(EventFilter{})
	defer events.Close()
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
	var adminUnpin *LeaseEvent
	deadline := time.Now().Add(3 * time.Second)
	for adminUnpin == nil && time.Now().Before(deadline) {
		select {
		case ev := <-events.C:
			if ev.Type == LeaseAdminUnpin {
				e := ev
				adminUnpin = &e
			}
		case <-time.After(50 * time.Millisecond):
		}
	}
	if adminUnpin == nil {
		t.Fatal("no admin_unpin event was emitted for the unpin-by-holder-prefix")
	}
	if adminUnpin.LeaseID != "-" {
		t.Fatalf("admin_unpin event lease id = %q, want \"-\"", adminUnpin.LeaseID)
	}
	if adminUnpin.Owner != "" {
		t.Fatalf("admin_unpin event owner = %q, want empty", adminUnpin.Owner)
	}
}

// TestAdminUnpinEventCarriesDashLeaseID pins the event surface directly:
// unpinning through the admin route emits exactly one admin_unpin event
// and its lease id is "-", never "", so every consumer's lease_id field
// stays well-formed (spoond-k0uz R4-1, L10).
func TestAdminUnpinEventCarriesDashLeaseID(t *testing.T) {
	ts, svc, _, _ := newAdminServer(t, "admin-tok")
	events := svc.Subscribe(EventFilter{})
	defer events.Close()
	l, err := svc.grant(context.Background(), "c", "py-base", time.Hour, true, "", nil, "pool:honey/work-9", "", nil)
	if err != nil {
		t.Fatalf("grant worker: %v", err)
	}
	if _, err := svc.setPinned("c", l.ID, true); err != nil {
		t.Fatalf("pin: %v", err)
	}
	resp, body := doReq(t, "POST", ts.URL+"/api/admin/unpin-by-holder?holder_prefix=pool:", "admin-tok", nil)
	if resp.StatusCode != http.StatusOK || body["unpinned"] != float64(1) {
		t.Fatalf("unpin by prefix = %d (%v), want 200 unpinned=1", resp.StatusCode, body)
	}
	var adminUnpin *LeaseEvent
	deadline := time.Now().Add(3 * time.Second)
	for adminUnpin == nil && time.Now().Before(deadline) {
		select {
		case ev := <-events.C:
			if ev.Type == LeaseAdminUnpin {
				e := ev
				adminUnpin = &e
			}
		case <-time.After(50 * time.Millisecond):
		}
	}
	if adminUnpin == nil {
		t.Fatal("no admin_unpin event was emitted")
	}
	if adminUnpin.LeaseID != "-" {
		t.Fatalf("admin_unpin event lease id = %q, want \"-\"", adminUnpin.LeaseID)
	}
}
