package api

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v2/store"
	"github.com/jrimmer/spoond/v2/substrate/e2b"
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

	bare, err := svc.grant(ctx, "c", "py-base", time.Minute, false, "", nil, "", "", nil)
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
	plain, err := svc.grant(ctx, "c", "py-base", time.Minute, false, "", nil, "", "", nil)
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

	l, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "", "", nil)
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
		Persistent: persistent, State: "lost", Class: "guaranteed",
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

// TestGCLostGraceExpiresDropsKeptRows: once a lost lease's grace
// period lapses, the pass also deletes its lease_kept_builds rows —
// only a release removed them before, and a lease stuck in lost is
// never released — so the builds behind the pins become reclaimable.
func TestGCLostGraceExpiresDropsKeptRows(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

	svc, buf, db := gcTestClock(t, now)
	pause, ckpt := lostChain(t, db)
	seedLostLease(t, db, "l-kept", pause, ckpt, false, now.Add(-25*time.Hour))
	if err := db.KeepBuild(context.Background(), "l-kept", ckpt, now.Add(-24*time.Hour)); err != nil {
		t.Fatalf("keep: %v", err)
	}
	// Inside the grace the keep row would hold the build; the lease's
	// resume/checkpoint builds are already past the grace here, so the
	// first pass is expected to propose them and delete the rows.
	if err := svc.gcOnce(context.Background()); err != nil {
		t.Fatalf("gc: %v", err)
	}
	rows, err := db.ListKeptBuilds(context.Background())
	if err != nil {
		t.Fatalf("list kept: %v", err)
	}
	if len(rows["l-kept"]) != 0 {
		t.Fatalf("kept rows of a grace-expired lost lease = %v, want gone", rows)
	}
	if !strings.Contains(buf.String(), "would delete "+ckpt) {
		t.Fatalf("grace-expired lost lease's kept build still protected:\n%s", buf.String())
	}

	// Sanity: a lost lease inside its grace period keeps its rows.
	svc, buf, db = gcTestClock(t, now)
	pause, ckpt = lostChain(t, db)
	seedLostLease(t, db, "l-kept2", pause, ckpt, false, now.Add(-time.Hour))
	if err := db.KeepBuild(context.Background(), "l-kept2", ckpt, now.Add(-time.Hour)); err != nil {
		t.Fatalf("keep: %v", err)
	}
	if !gcKeeps(t, svc, buf, ckpt) {
		t.Fatalf("a lost lease inside its grace lost its kept build:\n%s", buf.String())
	}
}

// TestGCStampsLostLeaseWithoutLostAt: a lost lease with an empty
// lost_at (lost before the column existed) gets its lost_at stamped by
// the first pass that sees it and keeps that stamp on every later pass,
// so the grace period starts once. Its builds stay kept roots while
// the stamped time plus the grace period is in the future and become
// candidates once it has passed.
func TestGCStampsLostLeaseWithoutLostAt(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

	svc, buf, db := gcTestClock(t, now)
	pause, ckpt := lostChain(t, db)
	seedLostLease(t, db, "l-nostamp", pause, ckpt, false, time.Time{})

	// The first pass counts as the moment of loss: the builds are kept
	// roots and the row leaves the pass stamped.
	if !gcKeeps(t, svc, buf, pause, ckpt) {
		t.Fatalf("an unstamped loss was not protected by the first pass:\n%s", buf.String())
	}
	row := leaseRow(t, db, "l-nostamp")
	if row.LostAt.IsZero() {
		t.Fatal("the first GC pass did not stamp lost_at")
	}
	if !row.LostAt.Equal(now) {
		t.Fatalf("stamped lost_at = %v, want the pass time %v", row.LostAt, now)
	}

	// A later pass reads the stored stamp: it neither rewrites it nor
	// restarts the grace period, and once the stamped time plus the
	// grace period has passed the builds are candidates.
	late := now.Add(23 * time.Hour)
	svc.now = func() time.Time { return late }
	buf.Reset()
	if err := svc.gcOnce(context.Background()); err != nil {
		t.Fatalf("gc: %v", err)
	}
	if strings.Contains(buf.String(), "would delete "+pause) || strings.Contains(buf.String(), "would delete "+ckpt) {
		t.Errorf("23 h after the stamp the builds were already reclaimable:\n%s", buf.String())
	}
	row = leaseRow(t, db, "l-nostamp")
	if !row.LostAt.Equal(now) {
		t.Fatalf("a later pass rewrote lost_at to %v, want %v", row.LostAt, now)
	}

	expired := now.Add(25 * time.Hour)
	svc.now = func() time.Time { return expired }
	buf.Reset()
	if err := svc.gcOnce(context.Background()); err != nil {
		t.Fatalf("gc: %v", err)
	}
	for _, id := range []string{pause, ckpt} {
		if !strings.Contains(buf.String(), "would delete "+id) {
			t.Errorf("25 h after the stamp %s was still held:\n%s", id, buf.String())
		}
	}
	row = leaseRow(t, db, "l-nostamp")
	if !row.LostAt.Equal(now) {
		t.Fatalf("the expired pass rewrote lost_at to %v, want %v", row.LostAt, now)
	}
}

// TestGCStampsLostLeaseWithoutLostAtLoaded: the stamp goes through the
// in-memory lease when the service has one (the setState path), not
// only through the stored row.
func TestGCStampsLostLeaseWithoutLostAtLoaded(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	svc, _, db := gcTestClock(t, now)
	pause, ckpt := lostChain(t, db)
	seedLostLease(t, db, "l-mem", pause, ckpt, true, time.Time{})
	if err := svc.LoadState(context.Background()); err != nil {
		t.Fatalf("load state: %v", err)
	}

	if _, err := svc.keptBuilds(context.Background()); err != nil {
		t.Fatalf("kept builds: %v", err)
	}
	row := leaseRow(t, db, "l-mem")
	if !row.LostAt.Equal(now) {
		t.Fatalf("persisted lost_at = %v, want the pass time %v", row.LostAt, now)
	}
	svc.store.mu.Lock()
	mem := svc.store.leases["l-mem"]
	svc.store.mu.Unlock()
	if mem == nil || !mem.LostAt.Equal(now) {
		t.Fatalf("in-memory lost_at = %v, want %v", mem.LostAt, now)
	}

	// The stamp survives a reload untouched and still bounds the grace
	// period on later passes.
	if _, err := svc.keptBuilds(context.Background()); err != nil {
		t.Fatalf("second kept builds: %v", err)
	}
	row = leaseRow(t, db, "l-mem")
	if !row.LostAt.Equal(now) {
		t.Fatalf("a second pass rewrote lost_at to %v, want %v", row.LostAt, now)
	}
}

// TestGCKeepsLostLeaseWithoutLostAt: a lost lease with an empty lost_at
// (lost before the column existed) counts as lost at the time of the
// first pass that sees it, so its snapshots are protected for the full
// grace period from that stamp — for a persistent lease and a plain one
// alike, and on a much later first pass too.
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
			row := leaseRow(t, db, "l-nostamp")
			if !row.LostAt.Equal(clock) {
				t.Errorf("persistent=%v at %v: lost_at = %v, want the pass time",
					tc.persistent, clock, row.LostAt)
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

// seedLostLeaseInMemory loads a running lease into the service, marks it
// lost with lostAt, and returns it. The lease is past its grace exactly
// when now-lostAt exceeds its period, so a later GC pass is expected to
// release it through the normal path.
func seedLostLeaseInMemory(t *testing.T, svc *Service, db *store.DB, persistent bool, lostAt time.Time) *Lease {
	t.Helper()
	l, err := svc.grant(context.Background(), "consumer-a", "py-base", time.Minute, persistent, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	svc.store.mu.Lock()
	l.setState("lost")
	l.LostAt = lostAt
	svc.saveLeaseLocked(l)
	svc.store.mu.Unlock()
	return l
}

// TestGCLostGraceReleasesPastGrace: a lost lease past its grace period
// (24 h plain, 7 d persistent) is released by the GC pass through the
// normal release path; one still inside its grace is kept.
func TestGCLostGraceReleasesPastGrace(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name       string
		persistent bool
		age        time.Duration
		wantGone   bool
	}{
		{"plain inside", false, 23 * time.Hour, false},
		{"plain past", false, 25 * time.Hour, true},
		{"persistent inside", true, 6 * 24 * time.Hour, false},
		{"persistent past", true, 8 * 24 * time.Hour, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, db, _ := newTestService(t)
			seedImage(t, db, "py-base", 2048)
			l := seedLostLeaseInMemory(t, svc, db, tc.persistent, now.Add(-tc.age))
			if err := svc.gcOnce(context.Background()); err != nil {
				t.Fatalf("gc: %v", err)
			}
			svc.store.mu.Lock()
			_, present := svc.store.leases[l.ID]
			svc.store.mu.Unlock()
			if present == tc.wantGone {
				t.Fatalf("lease present=%v after GC, want gone=%v", present, tc.wantGone)
			}
		})
	}
}

// TestGCLostGraceLeavesRecoveredLease: a lease that left the lost state
// (recovered, so lost_at is cleared) is never touched by the release
// sweep, however old the loss once was.
func TestGCLostGraceLeavesRecoveredLease(t *testing.T) {
	now := time.Now()
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	l := seedLostLeaseInMemory(t, svc, db, false, now.Add(-25*time.Hour))

	// The lease is recovered: setState clears lost_at.
	svc.store.mu.Lock()
	l.setState("recovered")
	svc.saveLeaseLocked(l)
	svc.store.mu.Unlock()

	if err := svc.gcOnce(context.Background()); err != nil {
		t.Fatalf("gc: %v", err)
	}
	svc.store.mu.Lock()
	_, present := svc.store.leases[l.ID]
	svc.store.mu.Unlock()
	if !present {
		t.Fatal("a recovered lease was released by the lost-grace sweep")
	}
}

// TestGCLostGraceReleasesAndFreesQuota: a lost lease past its grace
// still counted against its owner's lease cap, so a create was refused
// 429; the GC pass releases it (emitting a released event with reason
// lost_expired) and the next create succeeds.
func TestGCLostGraceReleasesAndFreesQuota(t *testing.T) {
	srv, h, tok, _ := newMemQuotaServer(t, map[string]int{"py-base": 2048}, `{"max_leases":1}`)
	svc := srv.svc

	rec, body := createSandboxAs(t, h, tok, "py-base")
	if rec.Code != http.StatusCreated {
		t.Fatalf("first create = %d %s", rec.Code, rec.Body.String())
	}
	id, _ := body["id"].(string)
	if id == "" {
		t.Fatalf("no id in %v", body)
	}

	svc.store.mu.Lock()
	l := svc.store.leases[id]
	l.setState("lost")
	l.LostAt = time.Now().Add(-25 * time.Hour)
	svc.saveLeaseLocked(l)
	svc.store.mu.Unlock()

	// The lost lease still holds the count quota.
	rec, _ = createSandboxAs(t, h, tok, "py-base")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("create while lost lease held quota = %d, want 429", rec.Code)
	}

	// The GC pass releases it and says why on the event stream.
	all := svc.Subscribe(EventFilter{LeaseID: id})
	if err := svc.gcOnce(context.Background()); err != nil {
		all.Close()
		t.Fatalf("gc: %v", err)
	}
	all.Close()
	released := false
	for _, ev := range collectEvents(all.C) {
		if ev.Type == LeaseReleased {
			released = true
			if ev.Detail != lostReleaseReason {
				t.Errorf("released detail = %q, want %q", ev.Detail, lostReleaseReason)
			}
		}
	}
	if !released {
		t.Fatal("no released event for the grace-expired lost lease")
	}

	// The quota is free now.
	rec, _ = createSandboxAs(t, h, tok, "py-base")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create after the release = %d, want 201", rec.Code)
	}
}
