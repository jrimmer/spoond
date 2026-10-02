package api

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jrimmer/spoond/store"
	"github.com/jrimmer/spoond/substrate/e2b"
)

// The lost-lease snapshot grace (owner decision 2026-10-02): a lease
// whose sandbox died in a substrate crash keeps its resume and
// checkpoint builds for a grace period after the loss — 7 days for a
// persistent lease, 1 day for any other — and only then may the GC
// reclaim them. lost_at is the clock the grace period counts from, and
// spoond doctor warns about every lost lease while it holds.

// leaseRow reads one lease row back from the store.
func leaseRow(t *testing.T, db *store.DB, id string) store.LeaseRow {
	t.Helper()
	rows, err := db.ListLeases(context.Background())
	if err != nil {
		t.Fatalf("list leases: %v", err)
	}
	for _, r := range rows {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("lease %s not found", id)
	return store.LeaseRow{}
}

// TestReconcileCrashStampsLostAtOnce: the crash reconcile stamps lost_at
// when a lease becomes lost, never overwrites an existing stamp, and a
// lease that leaves the lost state has the stamp cleared.
func TestReconcileCrashStampsLostAtOnce(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	bare, err := svc.grant(ctx, "c", "py-base", time.Minute, false, "", nil)
	if err != nil {
		t.Fatalf("grant bare: %v", err)
	}
	// A lease already carrying a stamp (lost once before) keeps it: the
	// grace period counts from the first loss.
	first := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	svc.store.mu.Lock()
	bare.LostAt = first
	svc.store.mu.Unlock()

	sub.Fake.Kill(bare.SandboxID)
	if summary := svc.reconcileCrash(ctx); summary.Lost != 1 {
		t.Fatalf("summary = %+v, want one loss", summary)
	}
	if !bare.LostAt.Equal(first) {
		t.Fatalf("LostAt = %v, want the existing stamp %v", bare.LostAt, first)
	}
	row := leaseRow(t, db, bare.ID)
	if !row.LostAt.Equal(first) {
		t.Fatalf("persisted lost_at = %v, want %v", row.LostAt, first)
	}

	// A lease lost with no stamp records the loss time.
	plain, err := svc.grant(ctx, "c", "py-base", time.Minute, false, "", nil)
	if err != nil {
		t.Fatalf("grant plain: %v", err)
	}
	sub.Fake.Kill(plain.SandboxID)
	if summary := svc.reconcileCrash(ctx); summary.Lost != 1 {
		t.Fatalf("second summary = %+v, want one loss", summary)
	}
	if plain.LostAt.IsZero() {
		t.Fatal("a lease becoming lost was not stamped")
	}

	// A lease that leaves the lost state clears the stamp.
	svc.store.mu.Lock()
	plain.setState("running")
	svc.saveLeaseLocked(plain)
	svc.store.mu.Unlock()
	row = leaseRow(t, db, plain.ID)
	if !row.LostAt.IsZero() {
		t.Fatalf("a lease leaving the lost state kept lost_at = %v", row.LostAt)
	}
}

// TestUndrainFailureStampsLostAt: a lease whose resume fails during
// undrain becomes lost with lost_at stamped and persisted.
func TestUndrainFailureStampsLostAt(t *testing.T) {
	ts, svc, db, sub := newAdminServer(t, "admin-tok")
	ctx := context.Background()

	l, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	resp, _ := doReq(t, "POST", ts.URL+"/api/admin/drain", "admin-tok", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("drain = %d", resp.StatusCode)
	}
	// The pool refills between the drain and the undrain, so fail every
	// Create: the resume cannot succeed.
	sub.FailCall("Create", 0, context.DeadlineExceeded)

	resp, body := doReq(t, "POST", ts.URL+"/api/admin/undrain", "admin-tok", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("undrain = %d (%v)", resp.StatusCode, body)
	}
	if failed, ok := body["failed"].([]any); !ok || len(failed) != 1 {
		t.Fatalf("undrain failed = %v, want one entry", body["failed"])
	}
	if l.State != "lost" {
		t.Fatalf("lease state = %q, want lost", l.State)
	}
	if l.LostAt.IsZero() {
		t.Fatal("the lost lease was not stamped")
	}
	row := leaseRow(t, db, l.ID)
	if row.LostAt.IsZero() {
		t.Fatal("lost_at was not persisted")
	}
}

// seedLostLease writes one lost lease whose resume and checkpoint builds
// are pause and ckpt, with the given lost_at (zero = the empty column,
// lost before this change existed) and persistence.
func seedLostLease(t *testing.T, db *store.DB, id, pause, ckpt string, persistent bool, lostAt time.Time) {
	t.Helper()
	if err := db.UpsertLease(context.Background(), store.LeaseRow{
		ID: id, Owner: "consumer-a", Image: "py-base",
		ResumeBuildID: pause, LastCheckpointBuildID: ckpt,
		Persistent: persistent, State: "lost",
		CreatedAt: gcOld, ExpiresAt: gcOld, LastActive: gcOld, LostAt: lostAt,
	}); err != nil {
		t.Fatalf("seed lease %s: %v", id, err)
	}
}

// gcTestClock returns a GC test service whose clock is frozen at now.
func gcTestClock(t *testing.T, now time.Time) (*Service, *bytes.Buffer, *store.DB) {
	t.Helper()
	svc, buf, db, _ := gcTestService(t)
	svc.now = func() time.Time { return now }
	return svc, buf, db
}

// lostChain seeds an image with its own current build plus a pause build
// and a checkpoint build no other row names, and returns them: only the
// lost lease's grace period can keep them.
func lostChain(t *testing.T, db *store.DB) (pause, ckpt string) {
	t.Helper()
	tid := e2b.NewTemplateID()
	root := e2b.NewUUID()
	seedGCImage(t, db, "py-base", root)
	pause, ckpt = e2b.NewUUID(), e2b.NewUUID()
	seedGCBuild(t, db, root, "template", "", "", "ready", tid)
	seedGCBuild(t, db, pause, "pause", root, "consumer-a", "ready", tid)
	seedGCBuild(t, db, ckpt, "checkpoint", root, "consumer-a", "ready", tid)
	return pause, ckpt
}

// gcKeeps runs one GC pass (dry run) and reports whether every named
// build stayed out of the candidate list.
func gcKeeps(t *testing.T, svc *Service, buf *bytes.Buffer, ids ...string) bool {
	t.Helper()
	buf.Reset()
	if err := svc.gcOnce(context.Background()); err != nil {
		t.Fatalf("gc: %v", err)
	}
	for _, id := range ids {
		if strings.Contains(buf.String(), "would delete "+id) {
			return false
		}
	}
	return true
}

// TestGCKeepsLostPersistentLeaseGrace: a persistent lost lease's resume
// and checkpoint builds stay kept roots 6 days after lost_at and become
// candidates 8 days after it.
func TestGCKeepsLostPersistentLeaseGrace(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

	svc, buf, db := gcTestClock(t, now)
	pause, ckpt := lostChain(t, db)
	seedLostLease(t, db, "l-per", pause, ckpt, true, now.Add(-6*24*time.Hour))
	if !gcKeeps(t, svc, buf, pause, ckpt) {
		t.Errorf("a 6-day-old persistent loss was already reclaimable:\n%s", buf.String())
	}

	svc, buf, db = gcTestClock(t, now)
	pause, ckpt = lostChain(t, db)
	seedLostLease(t, db, "l-per", pause, ckpt, true, now.Add(-8*24*time.Hour))
	if gcKeeps(t, svc, buf, pause, ckpt) {
		t.Errorf("an 8-day-old persistent loss still holds its snapshots:\n%s", buf.String())
	}
}

// TestGCKeepsLostNonPersistentLeaseGrace: the same at the 1-day period
// of a non-persistent lease — kept at 23 h, reclaimed at 25 h.
func TestGCKeepsLostNonPersistentLeaseGrace(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

	svc, buf, db := gcTestClock(t, now)
	pause, ckpt := lostChain(t, db)
	seedLostLease(t, db, "l-plain", pause, ckpt, false, now.Add(-23*time.Hour))
	if !gcKeeps(t, svc, buf, pause, ckpt) {
		t.Errorf("a 23-hour-old loss was already reclaimable:\n%s", buf.String())
	}

	svc, buf, db = gcTestClock(t, now)
	pause, ckpt = lostChain(t, db)
	seedLostLease(t, db, "l-plain", pause, ckpt, false, now.Add(-25*time.Hour))
	if gcKeeps(t, svc, buf, pause, ckpt) {
		t.Errorf("a 25-hour-old loss still holds its snapshots:\n%s", buf.String())
	}
}

// TestGCKeepsLostLeaseWithoutLostAt: a lost lease with an empty lost_at
// (lost before the column existed) counts as lost at the time of each
// pass, so its snapshots are protected for the full grace period
// starting now — for a persistent lease and a plain one alike, and on a
// much later pass too: the pre-change rows never age out on their own.
func TestGCKeepsLostLeaseWithoutLostAt(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		persistent bool
	}{
		{persistent: true},
		{persistent: false},
	} {
		for _, clock := range []time.Time{now, now.Add(30 * 24 * time.Hour)} {
			svc, buf, db := gcTestClock(t, clock)
			pause, ckpt := lostChain(t, db)
			seedLostLease(t, db, "l-nostamp", pause, ckpt, tc.persistent, time.Time{})
			if !gcKeeps(t, svc, buf, pause, ckpt) {
				t.Errorf("persistent=%v at %v: an unstamped loss was not protected from this pass:\n%s",
					tc.persistent, clock, buf.String())
			}
		}
	}
}

// TestGCLostGraceConfigured: the periods come from the service config —
// GC_LOST_GRACE_PERSISTENT and GC_LOST_GRACE in the backend — and the
// defaults apply when they are unset.
func TestGCLostGraceConfigured(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	svc, _, _ := gcTestClock(t, now)
	if p, o := svc.lostGraces(); p != 7*24*time.Hour || o != 24*time.Hour {
		t.Fatalf("default graces = %v/%v, want 168h/24h", p, o)
	}
	svc.cfg.LostGracePersistent = time.Hour
	svc.cfg.LostGrace = 2 * time.Hour
	if p, o := svc.lostGraces(); p != time.Hour || o != 2*time.Hour {
		t.Fatalf("configured graces = %v/%v, want 1h/2h", p, o)
	}
}
