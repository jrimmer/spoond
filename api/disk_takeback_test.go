package api

// Disk take-back tests (FS3a): diskTakeBack runs against a fake
// substrate and a temp storage path, so every byte number is the test's
// own. The fair-share box is shaped like newFairShareService's: a 1 GiB
// volume, two owners, and per-owner recorded disk bytes from the
// catalog.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jrimmer/spoond/v2/store"
	"github.com/jrimmer/spoond/v2/substrate"
)

// diskTBHarness is one Service with a 1 GiB snapshot volume (total and
// free both under the test's control), a temp storage path and a clean
// fair-share cache.
type diskTBHarness struct {
	svc   *Service
	db    *store.DB
	sub   *testSub
	total uint64
	free  uint64
}

// newDiskTBHarness builds the harness: two legacy-token owners
// (consumer-a via token-a, consumer-b via token-b, the default
// newTestService tokens), a 1 GiB volume whose free bytes start at the
// given number, and no accounted builds.
func newDiskTBHarness(t *testing.T, free uint64) *diskTBHarness {
	t.Helper()
	svc, db, sub := newTestService(t)
	svc.cfg.TemplateStoragePath = t.TempDir()
	svc.diskCapacity = func(string) (uint64, uint64, error) { return 1 << 30, free, nil }
	// Warm the node-info cache the fair-share view reads: a cold cache
	// would report unknown capacity and compute no slices.
	sub.SetNodeInfo(substrate.NodeInfo{
		Status:            "healthy",
		HugepagesTotal:    512,
		HugepageSizeBytes: 2 << 20,
	}, nil)
	svc.updateNodeMetrics(context.Background())
	svc.invalidateFairShares()
	return &diskTBHarness{svc: svc, db: db, sub: sub, total: 1 << 30, free: free}
}

// seedPausedLease records a suspended lease with a pause build of the
// given size, as the disk-usage query and pausedLeasesOf both read it.
// pausedAtBack ages the pause date.
func (h *diskTBHarness) seedPausedLease(t *testing.T, id, owner string, size int64, pausedAtBack time.Duration, pinned bool) {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	buildID := "pause-" + id
	if err := h.db.InsertBuild(ctx, store.BuildRow{
		BuildID: buildID, Kind: "pause", ParentBuildID: id, Owner: owner,
		State: "ready", SizeBytes: size, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("insert pause build: %v", err)
	}
	if err := h.db.UpsertLease(ctx, store.LeaseRow{
		ID: id, Owner: owner, Image: "py-base", State: "suspended",
		CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastActive: now,
		ResumeBuildID: buildID, Pinned: pinned, PausedAt: now.Add(-pausedAtBack),
		Class: "guaranteed",
	}); err != nil {
		t.Fatalf("insert lease: %v", err)
	}
}

// seedNamedAndKept gives owner a named snapshot and a kept checkpoint of
// the given sizes, the two kinds take-back must never touch.
func (h *diskTBHarness) seedNamedAndKept(t *testing.T, owner string, namedSize, keptSize int64) {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	if _, err := h.db.InsertNamedSnapshot(ctx, store.NamedSnapshotRow{
		Owner: owner, Name: "keepme", BuildID: "named-" + owner,
		SourceLeaseID: "src", Image: "py-base", ImageBuildID: "img-1",
		MemoryMB: 1024, SizeBytes: namedSize, CreatedAt: now,
	}, 1); err != nil {
		t.Fatalf("insert named snapshot: %v", err)
	}
	keptID := "kept-" + owner
	if err := h.db.InsertBuild(ctx, store.BuildRow{
		BuildID: keptID, Kind: "checkpoint", Owner: owner, State: "ready",
		SizeBytes: keptSize, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("insert kept build: %v", err)
	}
	if err := h.db.UpsertLease(ctx, store.LeaseRow{
		ID: "lkc-" + owner, Owner: owner, Image: "py-base", State: "running",
		CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastActive: now,
		Class: "guaranteed",
	}); err != nil {
		t.Fatalf("insert kept lease: %v", err)
	}
	if err := h.db.KeepBuild(ctx, "lkc-"+owner, keptID, now); err != nil {
		t.Fatalf("keep build: %v", err)
	}
}

// drainEvents collects the event stream into (type, detail) pairs.
func drainEvents(t *testing.T, svc *Service) []LeaseEvent {
	t.Helper()
	sub := svc.Subscribe(EventFilter{})
	t.Cleanup(sub.Close)
	var out []LeaseEvent
	for {
		select {
		case ev, ok := <-sub.C:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-time.After(200 * time.Millisecond):
			return out
		}
	}
}

// mkAgedOrphanDir creates a UUID-named directory past the orphan
// reap's minimum age, the shape reapOrphans quarantines.
func mkAgedOrphanDir(t *testing.T, root, name string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "memfile"), make([]byte, 1<<20), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	return dir
}

// GC_DELETE stays the hard guard even on the take-back path: an unset
// or off GC never deletes, so the reap frees nothing and the call moves
// on to leases (or box_full). Here no lease exists, so the answer is
// the refusal and the orphan directory survives.
func TestDiskTakeBackGarbageHonoursORPHANREAP(t *testing.T) {
	h := newDiskTBHarness(t, 1<<30)
	orphan := mkAgedOrphanDir(t, h.svc.cfg.TemplateStoragePath, orphanName(t))
	t.Setenv("ORPHAN_REAP", "off")
	// A paused over-slice lease of the other owner: with the reap off
	// nothing may free it either.
	h.seedPausedLease(t, "vict-1", "consumer-b", 1<<30, 24*time.Hour, false)

	// Need more than the room the box has after its floor: with garbage
	// barred and the only candidate unpinned-but-protected by ORPHAN_REAP
	// ... the lease itself is NOT protected by ORPHAN_REAP (that guard
	// governs spoond's own garbage only), so take-back releases the
	// lease. Pin it via a second, pinned lease instead: nothing at all
	// can be taken, so the answer is box_full with the orphan intact.
	h.seedPausedLease(t, "pinned-1", "consumer-b", 1<<30, 24*time.Hour, true)
	if err := h.db.DeleteLease(context.Background(), "vict-1"); err != nil {
		t.Fatalf("drop vict-1: %v", err)
	}

	freed, err := h.svc.diskTakeBack(context.Background(), 1<<29, "consumer-a")
	if !isBoxFull(err) {
		t.Fatalf("err = %v, want box_full", err)
	}
	if freed != 0 {
		t.Fatalf("freed = %d, want 0 (off reaps nothing)", freed)
	}
	if _, err := os.Stat(orphan); err != nil {
		t.Fatalf("ORPHAN_REAP=off must leave the orphan alone: %v", err)
	}
}

// Garbage runs before any lease: with an orphan on disk and a paused
// over-slice lease of another owner, the orphan alone covers the need
// (its quarantined directory is backdated past the quarantine age so
// the same pass purges it) and no lease is released. One disk.cleanup
// event carries the bytes per category.
func TestDiskTakeBackGarbageFirst(t *testing.T) {
	h := newDiskTBHarness(t, 1<<30)
	t.Setenv("ORPHAN_REAP", "quarantine")
	orphan := mkAgedOrphanDir(t, h.svc.cfg.TemplateStoragePath, orphanName(t))
	// The victim-owner's paused lease is huge — if the lease were
	// touched at all, its row would be gone below.
	h.seedPausedLease(t, "vict-1", "consumer-b", 1<<30, 24*time.Hour, false)

	// Quarantine the orphan with a pass, then backdate its marker past
	// the quarantine age so take-back's own pass may purge it.
	h.svc.reapGarbageForRoom(context.Background())
	if !dirExists(orphan) {
		t.Fatal("precondition: orphan was not quarantined")
	}
	qdir := filepath.Join(h.svc.quarantineDir(), filepath.Base(orphan))
	old := time.Now().Add(-48 * time.Hour)
	if err := writeQuarantineMarker(qdir, old); err != nil {
		t.Fatalf("backdate marker: %v", err)
	}

	freed, err := h.svc.diskTakeBack(context.Background(), 512<<10, "consumer-a")
	if err != nil {
		t.Fatalf("diskTakeBack: %v", err)
	}
	if freed <= 0 {
		t.Fatalf("freed = %d, want the orphan's bytes", freed)
	}
	if dirExists(qdir) {
		t.Fatal("quarantined orphan survived the take-back pass")
	}
	// The over-slice owner's lease survived: garbage covered the need.
	rows, err := h.db.ListLeases(context.Background())
	if err != nil {
		t.Fatalf("list leases: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "vict-1" {
		t.Fatalf("leases after take-back = %v, want vict-1 intact", rows)
	}
	evs := drainEvents(t, h.svc)
	var cleanup *LeaseEvent
	var critical int
	for i := range evs {
		switch evs[i].Type {
		case LeaseDiskCleanup:
			cleanup = &evs[i]
		case LeaseCriticalRelease:
			critical++
		}
	}
	if cleanup == nil {
		t.Fatalf("no disk.cleanup event among %v", evs)
	}
	if !strings.Contains(cleanup.Detail, "0 of paused leases") && !strings.Contains(cleanup.Detail, "0 B") {
		if !strings.Contains(cleanup.Detail, "of paused leases") {
			t.Fatalf("cleanup detail %q lacks the per-category split", cleanup.Detail)
		}
	}
	if critical != 0 {
		t.Fatalf("garbage alone covered the need: %d critical_release events", critical)
	}
}

// Named snapshots and kept checkpoints are never deleted and never
// released: an over-slice owner whose whole recorded usage is named +
// kept has no candidates, so the call ends in box_full and the builds
// survive.
func TestDiskTakeBackNamedKeptUntouched(t *testing.T) {
	h := newDiskTBHarness(t, 1<<30)
	h.seedNamedAndKept(t, "consumer-b", 600<<20, 500<<20)

	freed, err := h.svc.diskTakeBack(context.Background(), 1<<20, "consumer-a")
	if !isBoxFull(err) {
		t.Fatalf("err = %v, want box_full", err)
	}
	if freed != 0 {
		t.Fatalf("freed = %d, want 0", freed)
	}
	// Both builds are still in the catalog.
	builds, err := h.db.ListBuilds(context.Background())
	if err != nil {
		t.Fatalf("list builds: %v", err)
	}
	if len(builds) != 1 || builds[0].BuildID != "kept-consumer-b" {
		t.Fatalf("builds = %v, want kept-consumer-b intact", builds)
	}
	snap, err := h.db.GetNamedSnapshot(context.Background(), "consumer-b", "keepme", 1)
	if err != nil {
		t.Fatalf("named snapshot gone: %v", err)
	}
	if snap.BuildID != "named-consumer-b" {
		t.Fatalf("named snapshot = %+v", snap)
	}
	for _, ev := range drainEvents(t, h.svc) {
		if ev.Type == LeaseCriticalRelease {
			t.Fatalf("critical_release for named/kept owner: %+v", ev)
		}
	}
}

// A paused pinned lease of the biggest borrower survives disk
// take-back: the call refuses with a boxFullError naming the requester
// and emits no critical_release for the pinned lease.
func TestDiskTakeBackPinnedSurvivesBoxFull(t *testing.T) {
	h := newDiskTBHarness(t, 1<<30)
	h.seedPausedLease(t, "pinned-1", "consumer-b", 800<<20, time.Hour, true)
	h.seedPausedLease(t, "plain-1", "consumer-a", 10<<20, 2*time.Hour, false)

	// consumer-a asks for more than the box can free: plain-1 is the
	// requester's own lease (never a victim), pinned-1 is unpickable.
	_, err := h.svc.diskTakeBack(context.Background(), 1<<30, "consumer-a")
	var bf *boxFullError
	if !errorsAsBoxFull(err, &bf) {
		t.Fatalf("err = %v, want *boxFullError", err)
	}
	if bf.Owner != "consumer-a" {
		t.Fatalf("boxFullError owner = %q, want the requester", bf.Owner)
	}
	rows, err := h.db.ListLeases(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("leases = %v, want both intact", rows)
	}
	for _, ev := range drainEvents(t, h.svc) {
		if ev.Type == LeaseCriticalRelease && ev.LeaseID == "pinned-1" {
			t.Fatal("critical_release must never fire for a pinned lease")
		}
	}
}

// Running leases are never taken: the biggest borrower's only lease is
// running (its bytes are recorded against the owner), so nothing is
// reclaimable and the answer is box_full with the lease untouched.
func TestDiskTakeBackRunningUntouched(t *testing.T) {
	h := newDiskTBHarness(t, 1<<30)
	now := time.Now()
	buildID := "pause-run"
	if err := h.db.InsertBuild(context.Background(), store.BuildRow{
		BuildID: buildID, Kind: "pause", ParentBuildID: "run-1", Owner: "consumer-b",
		State: "ready", SizeBytes: 700 << 20, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("insert build: %v", err)
	}
	// A RUNNING lease whose resume build still points at the old pause
	// build: its bytes count for the owner but the lease is not a
	// candidate.
	if err := h.db.UpsertLease(context.Background(), store.LeaseRow{
		ID: "run-1", Owner: "consumer-b", Image: "py-base", State: "running",
		CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastActive: now,
		ResumeBuildID: buildID, Class: "guaranteed",
	}); err != nil {
		t.Fatalf("insert lease: %v", err)
	}

	if _, err := h.svc.diskTakeBack(context.Background(), 1<<20, "consumer-a"); !isBoxFull(err) {
		t.Fatalf("err = %v, want box_full", err)
	}
	rows, err := h.db.ListLeases(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "run-1" || rows[0].State != "running" {
		t.Fatalf("leases = %v, want run-1 still running", rows)
	}
}

// Statfs lag: after a release the fake's free number stays put, so the
// freeing credit is what tells the loop the need now fits. No second
// deletion happens.
func TestDiskTakeBackStatfsLagNoSecondDeletion(t *testing.T) {
	h := newDiskTBHarness(t, 100<<20) // the box is genuinely tight
	h.seedPausedLease(t, "vict-1", "consumer-b", 200<<20, 3*time.Hour, false)
	h.seedPausedLease(t, "vict-2", "consumer-b", 100<<20, 2*time.Hour, false)

	// The first victim frees 200 MiB; ZFS reports nothing yet (free
	// stays 100 MiB) but the freeing credit covers the 150 MiB need.
	freed, err := h.svc.diskTakeBack(context.Background(), 150<<20, "consumer-a")
	if err != nil {
		t.Fatalf("diskTakeBack: %v", err)
	}
	if freed != 200<<20 {
		t.Fatalf("freed = %d, want only vict-1's %d (lag must not cause a second deletion)", freed, 200<<20)
	}
	rows, err := h.db.ListLeases(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "vict-2" {
		t.Fatalf("leases = %v, want vict-2 spared by the freeing credit", rows)
	}
}

// Nothing reclaimable: an over-slice owner with no paused lease and an
// empty garbage pass answers *boxFullError.
func TestDiskTakeBackNothingReclaimableBoxFull(t *testing.T) {
	h := newDiskTBHarness(t, 1<<30)
	// consumer-b is far over its slice on paper (a deleted pause build's
	// rows are gone, so actually its usage is 0 — give it kept bytes so
	// Used > slice with no pause candidate).
	h.seedNamedAndKept(t, "consumer-b", 0, 900<<20)

	freed, err := h.svc.diskTakeBack(context.Background(), 1<<20, "consumer-a")
	var bf *boxFullError
	if !errorsAsBoxFull(err, &bf) {
		t.Fatalf("err = %v, want *boxFullError", err)
	}
	if freed != 0 {
		t.Fatalf("freed = %d, want 0", freed)
	}
}

// The aligned candidate check: an owner is only taken while its ratio
// after giving up the candidate stays above the requester's after-request
// ratio. Owner a at 150/100 with a 50-byte lease would drop to exactly
// the requester's 1.0 — not strictly above — so nothing is taken.
func TestDiskVictimsAlignedWithMemVictims(t *testing.T) {
	owners := []diskOwner{
		{Owner: "req", UsedBytes: 50, SliceBytes: 100},
		{Owner: "a", UsedBytes: 150, SliceBytes: 100, Paused: []diskLease{dl("a1", 50, false, time.Hour)}},
	}
	// Requester after: (50+50)/100 = 1.0. Owner after a1: exactly 1.0 —
	// not above, so no pick.
	if v := diskVictims(owners, "req", 50); v != nil {
		t.Fatalf("victims = %v, want none (owner would drop to the requester's ratio)", victimIDs(v))
	}
	// A smaller need keeps the owner above after the pick.
	if v := diskVictims(owners, "req", 20); len(v) != 1 || v[0].ID != "a1" {
		t.Fatalf("victims = %v, want a1 (owner stays at 1.3 > 0.7)", v)
	}
	// And the inside-slice guard still holds: a candidate that would
	// pull the owner below 1 is refused even when the requester's ratio
	// would allow it.
	owners[1].UsedBytes = 120
	owners[0].UsedBytes = 0
	owners[0].SliceBytes = 300 // requester after: 30/300 = 0.1
	if v := diskVictims(owners, "req", 30); v != nil {
		t.Fatalf("victims = %v, want none (120-30 leaves the owner inside its slice)", victimIDs(v))
	}
}

// errorsAsBoxFull is errors.As narrowed to *boxFullError, without
// importing errors.As at every call site above.
func errorsAsBoxFull(err error, bf **boxFullError) bool {
	for err != nil {
		if b, ok := err.(*boxFullError); ok {
			*bf = b
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// orphanName returns a UUID-shaped directory name (the reap only
// considers those).
func orphanName(t *testing.T) string {
	t.Helper()
	return uuid.NewString()
}
