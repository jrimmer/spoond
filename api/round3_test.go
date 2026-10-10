package api

// Round-3 review of spoond-k0uz: regression tests for the probes the
// layer-3 review raised against f805043.
//
//   - R3-1: a migrated, pinned, non-persistent lease with a past TTL and
//     a future hold expiry survives the first 3.0 sweep (the migration
//     extended its TTL to the hold expiry).
//   - R3-2: box_full on re-admission answers 429 with code box_full on
//     POST /resume and resume-on-use (exec), and an undrain defers
//     instead of reporting resume_failed.
//   - R3-3: the one clock never releases a running lease (a stale
//     PausedAt on a running row, a lease resumed between collection and
//     release), and a suspended row with no pause date gets its clock at
//     load.
//   - R3-4: resuming a running lease is a no-op, not a snapshot restore
//     (restored from the deleted api/held_test.go, 81c93a1).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v2/store"
)

// seedPinnedRow inserts a lease row the shape migration 0022 leaves for
// a Honey flight: pinned, non-persistent, with the given expiry.
func seedPinnedRow(t *testing.T, svc *Service, id string, expiresAt time.Time) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	if err := svc.db.UpsertLease(ctx, store.LeaseRow{
		ID: id, Owner: "consumer-a", Image: "py-base",
		CreatedAt: now.Add(-2 * time.Hour), ExpiresAt: expiresAt,
		LastActive: now.Add(-time.Hour), State: "running",
		Class: ClassGuaranteed, Pinned: true,
	}); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

// TestSweepExpiredSparesMigratedPinnedTLLLease: the api-level half of
// the R3-1 fix. A pinned non-persistent row with a past TTL and a
// future hold is exactly what migration 0022 must extend (its store
// fixture asserts the UPDATE); loaded into a service and run through
// sweepExpired, the lease must still be there afterwards. Mutation:
// drop the migration's expires_at extension, which lets the sweep
// release the lease at once (the shape that would delete a Honey
// flight that outlived its TTL at the upgrade).
func TestSweepExpiredSparesMigratedPinnedTLLLease(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	// The migration's output for a live hold on a non-persistent lease
	// whose TTL the hold had already outlived: expires_at moved to the
	// hold expiry, an hour out.
	seedPinnedRow(t, svc, "migrated", time.Now().Add(time.Hour))
	// Its control: a pinned non-persistent lease the migration did not
	// extend (no hold) still expires on its own TTL.
	seedPinnedRow(t, svc, "plain", time.Now().Add(-time.Minute))

	if err := svc.LoadState(ctx); err != nil {
		t.Fatalf("load: %v", err)
	}
	if svc.lookup("consumer-a", "migrated") == nil {
		t.Fatal("the migrated lease did not load")
	}
	svc.sweepExpired(ctx)
	if svc.lookup("consumer-a", "migrated") == nil {
		t.Fatal("sweepExpired released a migrated pinned lease whose TTL the migration had extended to the hold expiry")
	}
	if svc.lookup("consumer-a", "plain") != nil {
		t.Fatal("sweepExpired kept a pinned non-persistent lease past its own TTL")
	}
}

// boxFullNode builds a node where one pinned burst lease holds every
// usable hugepage: any guaranteed admission — a create, or the
// re-admission a resume runs — is box_full. The victim is granted and
// paused before the filler fills the box, exactly the order a node
// fills in production.
func boxFullNode(t *testing.T) (*httptest.Server, *Service, *Lease, *Lease) {
	t.Helper()
	ts, svc, sub, ctx := newPreemptServer(t)
	// 768 pages of 2 MiB = 1536 MiB; each running sandbox counts 512
	// pages = 1024 MiB.
	installDynamicNode(t, svc, sub, 768, 0, 512)

	// The victim: a guaranteed lease, granted while there is room, then
	// paused. Its resume needs its 1024 MiB back.
	victim, err := svc.grantLease(ctx, leaseRequest{owner: "consumer-b", image: "mid", ttl: time.Hour, persistent: true})
	if err != nil {
		t.Fatalf("grant victim: %v", err)
	}
	if _, err := svc.pauseLease(ctx, victim, false); err != nil {
		t.Fatalf("pause victim: %v", err)
	}
	if !victim.Suspended {
		t.Fatal("the victim lease did not suspend")
	}

	// The pinned burst lease that takes the room back while the victim
	// is suspended: 1024 MiB free after the fill, and every take-back
	// candidate pinned.
	filler, err := svc.grantLease(ctx, leaseRequest{owner: "burst-a", image: "mid", ttl: time.Hour, burst: true})
	if err != nil {
		t.Fatalf("grant filler: %v", err)
	}
	if _, err := svc.setPinned("burst-a", filler.ID, true); err != nil {
		t.Fatalf("pin filler: %v", err)
	}
	// Sanity: a guaranteed admission of the victim's size is box_full now.
	dropNodeCache(svc)
	if err := svc.admitGuaranteed(ctx, "consumer-b", 1024); !isBoxFull(err) {
		t.Fatalf("setup: guaranteed admission err = %v, want box_full", err)
	}
	return ts, svc, victim, filler
}

// TestResumeBoxFullHTTP429: POST /resume of a suspended guaranteed lease
// on a node full of pinned burst leases answers 429 box_full (the same
// body a create gets), not a 500 a client reads as a permanent lease
// failure; the lease stays suspended and nothing was paused
// (spoond-k0uz R3-2). Mutation: drop the isBoxFull case in
// writeResumeRefusal, which answers 500 "resume failed".
func TestResumeBoxFullHTTP429(t *testing.T) {
	ts, svc, victim, filler := boxFullNode(t)

	resp, body := doReq(t, "POST", ts.URL+"/api/leases/"+victim.ID+"/resume", "token-b", nil)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("resume on a pinned-full node = %d (%v), want 429 box_full", resp.StatusCode, body)
	}
	if body["code"] != "box_full" {
		t.Fatalf("resume refusal code = %v, want box_full", body["code"])
	}
	if got := svc.lookup("consumer-b", victim.ID); got == nil || !got.Suspended {
		t.Fatalf("the refused resume changed the lease: %+v", got)
	}
	if filler.Suspended {
		t.Fatal("a refused resume paused the pinned filler")
	}
}

// TestExecResumeOnUseBoxFullHTTP429: the same setup through the
// resume-on-use path — an exec on the suspended lease — answers 429
// box_full and leaves the lease suspended (spoond-k0uz R3-2). Mutation:
// the same as TestResumeBoxFullHTTP429 (the exec path answers through
// writeResumeRefusal).
func TestExecResumeOnUseBoxFullHTTP429(t *testing.T) {
	ts, svc, victim, _ := boxFullNode(t)

	resp, body := doReq(t, "POST", ts.URL+"/api/leases/"+victim.ID+"/exec", "token-b",
		map[string]any{"cmd": "echo hi"})
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("exec resume-on-use on a pinned-full node = %d (%v), want 429 box_full", resp.StatusCode, body)
	}
	if body["code"] != "box_full" {
		t.Fatalf("exec refusal code = %v, want box_full", body["code"])
	}
	if got := svc.lookup("consumer-b", victim.ID); got == nil || !got.Suspended {
		t.Fatalf("the refused exec changed the lease: %+v", got)
	}
}

// TestUndrainBoxFullDefers: an undrain whose resume is refused box_full
// (a node full of pinned leases) defers — the lease stays Drained, no
// resume_failed stamp and nothing gets paused (spoond-k0uz R3-2).
// Mutation: drop errBoxFull from undrainAdmissionRefusal, which runs the
// failure through the resume_failed stamp (and would lose the lease).
func TestUndrainBoxFullDefers(t *testing.T) {
	ts, svc, sub, ctx := newPreemptServer(t)
	svc.cfg.UndrainResumeRetries = 0
	// 1536 pages of 2 MiB = 3072 MiB, 1024 MiB of it held outside
	// leases; each live sandbox counts 512 pages = 1024 MiB.
	installDynamicNode(t, svc, sub, 1536, 512, 512)

	// The lease to drain: guaranteed, 2048 MiB ("big"), so its
	// re-admission needs 2048 MiB.
	target, err := svc.grantLease(ctx, leaseRequest{owner: "consumer-a", image: "big", ttl: time.Hour, persistent: true})
	if err != nil {
		t.Fatalf("grant target: %v", err)
	}
	res, err := svc.drain(ctx)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if res.Paused != 1 || len(res.Failed) != 0 {
		t.Fatalf("drain = %+v, want 1 paused and no failures", res)
	}
	if !target.Drained || target.PausedAt.IsZero() {
		t.Fatalf("drain left the target in the wrong shape: %+v", target)
	}

	// While it is suspended, a pinned burst lease takes half the room:
	// 1024 MiB free, every take-back candidate pinned, and the target's
	// re-admission (2048 MiB) is box_full.
	filler, err := svc.grantLease(ctx, leaseRequest{owner: "burst-a", image: "mid", ttl: time.Hour, burst: true})
	if err != nil {
		t.Fatalf("grant filler: %v", err)
	}
	if _, err := svc.setPinned("burst-a", filler.ID, true); err != nil {
		t.Fatalf("pin filler: %v", err)
	}
	dropNodeCache(svc)
	if err := svc.admitGuaranteed(ctx, "consumer-a", 2048); !isBoxFull(err) {
		t.Fatalf("setup: guaranteed re-admission err = %v, want box_full", err)
	}

	res2 := svc.undrain(ctx)
	if len(res2.Failed) != 1 {
		t.Fatalf("undrain = %+v, want the one box_full resume reported", res2)
	}
	if target.SuspendReason == suspendReasonResumeFailed {
		t.Fatal("a box_full undrain stamped resume_failed; it must defer")
	}
	if !target.Drained {
		t.Fatal("a box_full undrain did not keep the lease Drained for a later attempt")
	}
	if got := svc.lookup("consumer-a", target.ID); got == nil || !got.Suspended {
		t.Fatalf("the deferred undrain changed the target: %+v", got)
	}
	if filler.Suspended {
		t.Fatal("the undrain paused the pinned filler")
	}
	_ = ts
}

// TestRunningLeaseWithStalePausedAtSurvivesSweep: a running lease with
// a PausedAt 31 days old is not released by the one clock. The shape is
// the rollback scenario: 3.0 paused it, a 2.9 binary resumed it (2.9
// never writes paused_at), the roll-forward does not rerun 0022, and
// the stale pause date must not delete the running VM (spoond-k0uz
// R3-3). Mutation: drop the suspended check in releasePausedLeases or
// in the pre-release re-check, which releases the running lease.
func TestRunningLeaseWithStalePausedAtSurvivesSweep(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	l, err := svc.grant(ctx, "c", "py-base", time.Hour, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	// Stale pause date, running lease.
	svc.store.mu.Lock()
	l.PausedAt = time.Now().Add(-31 * 24 * time.Hour)
	svc.store.mu.Unlock()

	now := time.Now()
	svc.notifyPausedExpiring(ctx, now)
	svc.releasePausedLeases(ctx, now)
	svc.sweepExpired(ctx)
	if svc.lookup("c", l.ID) == nil {
		t.Fatal("the one clock released a running lease over its stale pause date")
	}
	if l.Suspended {
		t.Fatal("the sweep paused the running lease")
	}
}

// TestSuspendedRowWithZeroPausedAtGetsClockAtLoad: a suspended row with
// no pause date — the mark of a pause a rolled-back 2.9 took, which
// never writes paused_at — gets PausedAt stamped at load time, so it is
// on the one clock from the roll-forward instead of never; and a
// running row's stale pause date is cleared at load (spoond-k0uz
// R3-3c). Mutation: revert the LoadState hygiene, which leaves the
// suspended row with no clock (never released) and the running row with
// its stale date.
func TestSuspendedRowWithZeroPausedAtGetsClockAtLoad(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()
	now := time.Now().UTC()

	// Suspended with no pause date: the 2.9 rollback's pause.
	if err := svc.db.UpsertLease(ctx, store.LeaseRow{
		ID: "susp", Owner: "consumer-a", Image: "py-base",
		CreatedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour),
		LastActive: now.Add(-time.Hour), State: "suspended", Suspended: true,
		Class: ClassGuaranteed,
	}); err != nil {
		t.Fatalf("seed susp: %v", err)
	}
	// Running with a stale pause date: the lease 2.9 resumed.
	if err := svc.db.UpsertLease(ctx, store.LeaseRow{
		ID: "run", Owner: "consumer-a", Image: "py-base",
		CreatedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour),
		LastActive: now.Add(-time.Hour), State: "running",
		PausedAt: now.Add(-40 * 24 * time.Hour), Class: ClassGuaranteed,
	}); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	if err := svc.LoadState(ctx); err != nil {
		t.Fatalf("load: %v", err)
	}
	susp := svc.lookup("consumer-a", "susp")
	run := svc.lookup("consumer-a", "run")
	if susp == nil || run == nil {
		t.Fatal("the seeded leases did not load")
	}
	if susp.PausedAt.IsZero() {
		t.Fatal("a suspended row with no pause date did not get its one-clock date at load")
	}
	if age := time.Since(susp.PausedAt); age < 0 || age > 5*time.Minute {
		t.Fatalf("susp's new clock is %v old, want stamped at load", age)
	}
	if !run.PausedAt.IsZero() || run.PausedExpiryNotified {
		t.Fatalf("a running row kept a stale pause date: paused_at=%v notified=%v", run.PausedAt, run.PausedExpiryNotified)
	}
	// The corrections are persisted, so a reload agrees.
	if err := svc.LoadState(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if susp = svc.lookup("consumer-a", "susp"); susp.PausedAt.IsZero() {
		t.Fatal("the stamped clock did not persist across a reload")
	}
	if run = svc.lookup("consumer-a", "run"); !run.PausedAt.IsZero() {
		t.Fatalf("the cleared pause date came back on reload: %v", run.PausedAt)
	}
}

// TestReleasePausedRacesResume: a lease resumed between the clock's
// collection pass and the release is not deleted while running
// (spoond-k0uz R3-3, the re-check). Mutation: call releaseBecause
// directly (the old shape), which releases the resumed lease.
func TestReleasePausedRacesResume(t *testing.T) {
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
	pausedAt := l.PausedAt

	// Collect the lease as due, as the sweep would, then resume it
	// before the release lands.
	var due *Lease
	svc.store.mu.Lock()
	for _, c := range svc.store.leases {
		if c.ID == l.ID {
			due = c
		}
	}
	svc.store.mu.Unlock()
	if due == nil {
		t.Fatal("the paused lease was not found")
	}
	if _, err := svc.resume(ctx, "c", l.ID); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if svc.releaseIfPausedExpired(ctx, due, pausedAt.Add(30*24*time.Hour), "paused_expired") {
		t.Fatal("the clock released a lease that was resumed after collection")
	}
	if got := svc.lookup("c", l.ID); got == nil {
		t.Fatal("the clock released a lease that was resumed after collection")
	}
	if l.Suspended {
		t.Fatal("the resumed lease was left suspended by the clock pass")
	}
	// The same re-check still releases one that stayed suspended.
	if _, err := svc.pauseLease(ctx, l, false); err != nil {
		t.Fatalf("re-pause: %v", err)
	}
	if !svc.releaseIfPausedExpired(ctx, l, l.PausedAt.Add(30*24*time.Hour), "paused_expired") {
		t.Fatal("releaseIfPausedExpired did not release a lease still past its deadline")
	}
	if svc.lookup("c", l.ID) != nil {
		t.Fatal("a lease still suspended past its deadline was not released")
	}
}

// TestResumeRunningLeaseIsNoop: resuming a lease that is already running
// must not restore its pause build again (that would roll the guest's
// memory back); it returns the lease unchanged. Restored regression test
// for 81c93a1 (the file that carried it was deleted in the FS5
// migration); R3-4. Mutation: make resumeLease's running check
// `if false && ...`, which restores the snapshot again and fails the
// create-count assertion.
func TestResumeRunningLeaseIsNoop(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()
	l, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := svc.suspend(ctx, "c", l.ID); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if _, err := svc.resume(ctx, "c", l.ID); err != nil {
		t.Fatalf("resume: %v", err)
	}
	creates := func() int {
		n := 0
		for _, c := range sub.Fake.CallLog() {
			if strings.HasPrefix(c, "Create ") {
				n++
			}
		}
		return n
	}
	before := creates()
	got, err := svc.resume(ctx, "c", l.ID)
	if err != nil {
		t.Fatalf("resume of a running lease: %v", err)
	}
	if got.State != "running" || got.Suspended {
		t.Fatalf("lease after no-op resume: state %q suspended %v", got.State, got.Suspended)
	}
	if n := creates(); n != before {
		t.Fatalf("resume of a running lease restored the snapshot again (%d creates, want %d)", n, before)
	}
}

// TestRollForwardResumesRolledBackSuspend: the whole R3-3 rollback
// story in one pass — 3.0's clock does not release a lease a rolled-back
// 2.9 resumed, and the suspend 2.9 took gets a clock on roll-forward,
// while an untouched pause date keeps counting from its original pause.
// Mutation: revert the load-time hygiene or the suspended-only sweep,
// either of which deletes the running lease 31 d after its stale pause.
func TestRollForwardResumesRolledBackSuspend(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	// 3.0 pauses two leases.
	a, err := svc.grant(ctx, "c", "py-base", time.Hour, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant a: %v", err)
	}
	b, err := svc.grant(ctx, "c", "py-base", time.Hour, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant b: %v", err)
	}
	if _, err := svc.pauseLease(ctx, a, false); err != nil {
		t.Fatalf("pause a: %v", err)
	}
	if _, err := svc.pauseLease(ctx, b, false); err != nil {
		t.Fatalf("pause b: %v", err)
	}
	oldPause := b.PausedAt

	// The rolled-back 2.9 resumes a (no paused_at write) and suspends a
	// fresh lease c (no paused_at write either): its saves carry no
	// pause dates at all.
	if _, err := svc.resume(ctx, "c", a.ID); err != nil {
		t.Fatalf("2.9 resume of a: %v", err)
	}
	c, err := svc.grant(ctx, "c", "py-base", time.Hour, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant c: %v", err)
	}
	if _, err := svc.pauseLease(ctx, c, false); err != nil {
		t.Fatalf("2.9 pause c: %v", err)
	}
	svc.store.mu.Lock()
	a.PausedAt, a.PausedExpiryNotified = time.Time{}, false
	c.PausedAt, c.PausedExpiryNotified = time.Time{}, false
	svc.saveLeaseLocked(a)
	svc.saveLeaseLocked(c)
	svc.store.mu.Unlock()

	// Roll-forward: LoadState repairs the rows, then the clock runs.
	if err := svc.LoadState(ctx); err != nil {
		t.Fatalf("roll-forward load: %v", err)
	}
	a = svc.lookup("c", a.ID)
	b = svc.lookup("c", b.ID)
	c = svc.lookup("c", c.ID)
	if a == nil || b == nil || c == nil {
		t.Fatal("leases missing after roll-forward")
	}
	if !a.PausedAt.IsZero() {
		t.Fatalf("the resumed lease kept a pause date after roll-forward: %v", a.PausedAt)
	}
	if c.PausedAt.IsZero() {
		t.Fatal("the lease 2.9 suspended got no one-clock date at roll-forward")
	}
	if age := time.Since(c.PausedAt); age < 0 || age > 5*time.Minute {
		t.Fatalf("c's new clock is %v old, want stamped at load", age)
	}
	if !b.PausedAt.Equal(oldPause) {
		t.Fatalf("b's untouched pause date moved: %v -> %v", oldPause, b.PausedAt)
	}

	// 29 d after the roll-forward nothing is due: the resumed a is not
	// on the clock at all, b's original pause is not 30 d old yet, and
	// c's fresh clock is not either. At 30 d after the roll-forward, b
	// (whose clock ran from its original pause) and c (whose clock the
	// roll-forward stamped) are both released; a survives.
	first := c.PausedAt.Add(29 * 24 * time.Hour)
	svc.notifyPausedExpiring(ctx, first)
	svc.releasePausedLeases(ctx, first)
	if svc.lookup("c", a.ID) == nil || svc.lookup("c", b.ID) == nil || svc.lookup("c", c.ID) == nil {
		t.Fatal("a lease was released 29 d after the roll-forward")
	}
	second := c.PausedAt.Add(30 * 24 * time.Hour)
	svc.notifyPausedExpiring(ctx, second)
	svc.releasePausedLeases(ctx, second)
	if svc.lookup("c", a.ID) == nil {
		t.Fatal("the one clock released the running lease 31 d after its stale pause")
	}
	if svc.lookup("c", c.ID) != nil {
		t.Fatal("the one clock did not release the 2.9-suspended lease 30 d after its roll-forward stamp")
	}
	if svc.lookup("c", b.ID) != nil {
		t.Fatal("the one clock did not release the lease paused 30 d ago")
	}
}
