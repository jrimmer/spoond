package api

// Disk take-back tests (FS3a): diskTakeBack runs against a fake
// substrate and a temp storage path, so every byte number is the test's
// own. The fair-share box is shaped like newFairShareService's: a 1 GiB
// volume, two consumer owners plus the legacy-token owner (three
// slices), and per-owner recorded disk bytes from the catalog. The
// harness loads the catalog into memory (LoadState) so the live-lease
// re-check inside releaseBecauseIf sees real leases, the way a running
// spoond does.
//
// The box is deliberately tight in every test: free bytes well under
// the 5 % floor plus a running lease's disk_mb reservation, so the need
// is genuinely short and take-back actually runs instead of returning
// early.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// newDiskTBHarness builds the harness: the default newTestService
// owners (consumer-a via token-a, consumer-b via token-b, and the
// legacy-token owner), a 1 GiB volume whose free bytes start at the
// given number, and no accounted builds. The lease catalog is loaded
// into memory, so a later take-back releases live leases (with real
// Suspended state and a SandboxID), not catalog rows alone.
func newDiskTBHarness(t *testing.T, free uint64) *diskTBHarness {
	t.Helper()
	svc, db, sub := newTestService(t)
	svc.cfg.TemplateStoragePath = t.TempDir()
	svc.diskCapacity = func(string) (uint64, uint64, error) { return 1 << 30, free, nil }
	if err := svc.LoadState(context.Background()); err != nil {
		t.Fatalf("load state: %v", err)
	}
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

// seedRunningLease records a running lease carrying diskMB of disk
// allowance, so the room formula's Reserved picks it up.
func (h *diskTBHarness) seedRunningLease(t *testing.T, id, owner string, diskMB int) {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	if err := h.db.UpsertLease(ctx, store.LeaseRow{
		ID: id, Owner: owner, Image: "py-base", State: "running",
		CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastActive: now,
		Class: "guaranteed", DiskMB: diskMB,
	}); err != nil {
		t.Fatalf("insert running lease: %v", err)
	}
}

// tightBox shapes the box so take-back really runs: fake free bytes,
// a running lease's disk_mb as the reservation and DISK_RESERVE_PCT.
// The running lease goes to the legacy-token owner so the two consumer
// owners stay symmetric. The fair-share cache is dropped so the next
// view reads the new rows.
func (h *diskTBHarness) tightBox(t *testing.T, free uint64, runningDiskMB int, floorPct string) {
	t.Helper()
	h.free = free
	h.svc.diskCapacity = func(string) (uint64, uint64, error) { return h.total, free, nil }
	h.seedRunningLease(t, "reserve-run", "legacy-consumer", runningDiskMB)
	if err := h.svc.LoadState(context.Background()); err != nil {
		t.Fatalf("load state: %v", err)
	}
	h.svc.invalidateFairShares()
	t.Setenv("DISK_RESERVE_PCT", floorPct)
}

// leaseRowMust fetches one lease row, failing the test when it is gone.
func (h *diskTBHarness) leaseRowMust(t *testing.T, id string) store.LeaseRow {
	t.Helper()
	r, err := h.db.GetLease(context.Background(), id)
	if err != nil {
		t.Fatalf("lease %s missing (released?): %v", id, err)
	}
	return r
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

// watchEvents subscribes to the event stream now and returns a watcher
// over it. Subscribe FIRST: the bus delivers only to live subscribers,
// so a drain after the fact would miss everything. Events are read from
// the subscription channel directly (the bus buffers them), and every
// assertion waits for the event it cares about with a deadline — a
// snapshot taken the moment a call returns races the channel and sees
// an empty buffer half the time.
func watchEvents(t *testing.T, svc *Service) *eventWatcher {
	t.Helper()
	sub := svc.Subscribe(EventFilter{})
	t.Cleanup(sub.Close)
	return &eventWatcher{C: sub.C}
}

// eventWatcher reads one subscription's channel and logs what it saw.
// waitFor blocks until a wanted event arrives (2 s deadline); all keeps
// every event read so far, so an absence check is made only after a
// waitFor proved the stream alive — disk.cleanup closes every take-back
// call, successful or refused, so it is the always-emitted event a test
// waits for before asserting something never fired.
type eventWatcher struct {
	C <-chan LeaseEvent

	mu   sync.Mutex
	seen []LeaseEvent
}

// waitFor reads C until pred matches, failing the test after 2 s. Every
// event read on the way — the match included — is kept in the log.
func (w *eventWatcher) waitFor(t *testing.T, want string, pred func(LeaseEvent) bool) LeaseEvent {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev, ok := <-w.C:
			if !ok {
				t.Fatalf("event stream closed while waiting for %s", want)
			}
			w.mu.Lock()
			w.seen = append(w.seen, ev)
			w.mu.Unlock()
			if pred(ev) {
				return ev
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s", want)
		}
	}
}

// all returns every event read so far (by waitFor or drain). Read it
// only after a waitFor: before that it says nothing about the stream.
func (w *eventWatcher) all() []LeaseEvent {
	w.mu.Lock()
	defer w.mu.Unlock()
	got := make([]LeaseEvent, len(w.seen))
	copy(got, w.seen)
	return got
}

// drain takes everything buffered right now into the log. The bus
// buffers per subscriber, so nothing is lost by reading late; it still
// proves nothing about arrival, which is what waitFor is for.
func (w *eventWatcher) drain() {
	for {
		select {
		case ev, ok := <-w.C:
			if !ok {
				return
			}
			w.mu.Lock()
			w.seen = append(w.seen, ev)
			w.mu.Unlock()
		default:
			return
		}
	}
}

// mkAgedOrphanDir creates a UUID-named directory holding sizeMiB MiB,
// past the orphan reap's minimum age — the shape reapOrphans
// quarantines.
func mkAgedOrphanDir(t *testing.T, root, name string, sizeMiB int64) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "memfile"), make([]byte, sizeMiB<<20), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	return dir
}

// GC_DELETE stays the hard guard even on the take-back path: an off
// ORPHAN_REAP never deletes, so the reap frees nothing. With nothing
// else reclaimable (a pinned lease only) the answer is the refusal and
// the orphan directory survives.
func TestDiskTakeBackGarbageHonoursORPHANREAP(t *testing.T) {
	h := newDiskTBHarness(t, 1<<30)
	t.Setenv("ORPHAN_REAP", "off")
	orphan := mkAgedOrphanDir(t, h.svc.cfg.TemplateStoragePath, orphanName(t), 1)
	// Only a pinned lease exists to take: with the reap off nothing at
	// all can be freed, so the answer is box_full with the orphan
	// intact.
	h.seedPausedLease(t, "pinned-1", "consumer-b", 1<<30, 24*time.Hour, true)
	h.tightBox(t, 100<<20, 60, "5")

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
// (it is pre-quarantined with a backdated marker so take-back's own
// pass purges it) and no lease is released. One disk.cleanup event
// carries the bytes per category exactly.
func TestDiskTakeBackGarbageFirst(t *testing.T) {
	h := newDiskTBHarness(t, 1<<30)
	t.Setenv("ORPHAN_REAP", "quarantine")
	orphan := mkAgedOrphanDir(t, h.svc.cfg.TemplateStoragePath, orphanName(t), 4)
	// The victim-owner's paused lease is huge — if the lease were
	// touched at all, its row would be gone below.
	h.seedPausedLease(t, "vict-1", "consumer-b", 1<<30, 24*time.Hour, false)

	// Quarantine the orphan with a pass, then backdate its marker past
	// the quarantine age so take-back's own pass may purge it. The
	// quarantine is a rename: the storage path no longer holds the
	// directory, the quarantine dir does.
	h.svc.reapGarbageForRoom(context.Background())
	qdir := filepath.Join(h.svc.quarantineDir(), filepath.Base(orphan))
	if dirExists(orphan) {
		t.Fatal("precondition: quarantine must move the orphan away")
	}
	if !dirExists(qdir) {
		t.Fatal("precondition: orphan was not quarantined")
	}
	// The quarantined directory is what the purge deletes — marker file
	// included — so its measured size is what the pass must report.
	orphanBytes, err := h.svc.diskUsage(qdir)
	if err != nil {
		t.Fatalf("measure quarantined orphan: %v", err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := writeQuarantineMarker(qdir, old); err != nil {
		t.Fatalf("backdate marker: %v", err)
	}
	// 91 MiB free, 40 MiB running reservation, 5 % floor: the room is
	// negative before the pass and the orphan's 4 MiB carries the
	// 3 MiB need exactly past it — no lease needed.
	h.tightBox(t, 91<<20, 40, "5")

	const need = 3 << 20
	evs := watchEvents(t, h.svc)
	freed, err := h.svc.diskTakeBack(context.Background(), need, "consumer-a")
	if err != nil {
		t.Fatalf("diskTakeBack: %v", err)
	}
	// Exactly the orphan's measured bytes (its directory blocks and its
	// file's), nothing more.
	if freed != orphanBytes {
		t.Fatalf("freed = %d, want the orphan's %d", freed, orphanBytes)
	}
	if dirExists(qdir) {
		t.Fatal("quarantined orphan survived the take-back pass")
	}
	// The over-slice owner's lease survived: garbage covered the need.
	if r := h.leaseRowMust(t, "vict-1"); r.State != "suspended" {
		t.Fatalf("vict-1 = %v, want it intact", r)
	}
	// Wait for the pass's disk.cleanup (every completed call emits one)
	// so the stream is proven live before asserting what never fired.
	cleanup := evs.waitFor(t, "disk.cleanup", func(ev LeaseEvent) bool {
		return ev.Type == LeaseDiskCleanup
	})
	// The categories are named exactly: the orphan's 4 MiB of garbage
	// and zero paused-lease bytes.
	if !containsAll(cleanup.Detail, "4.0 MiB of garbage", "0 KiB of paused leases") {
		t.Fatalf("cleanup detail %q lacks the exact per-category split", cleanup.Detail)
	}
	for _, ev := range evs.all() {
		if ev.Type == LeaseCriticalRelease {
			t.Fatalf("garbage alone covered the need: critical_release %+v", ev)
		}
	}
}

// Named snapshots and kept checkpoints are never deleted and never
// released: an over-slice owner whose whole recorded usage is named +
// kept has no candidates, so the call ends in box_full and the builds
// survive.
func TestDiskTakeBackNamedKeptUntouched(t *testing.T) {
	h := newDiskTBHarness(t, 1<<30)
	h.seedNamedAndKept(t, "consumer-b", 600<<20, 500<<20)
	h.tightBox(t, 100<<20, 60, "5")

	evs := watchEvents(t, h.svc)
	freed, err := h.svc.diskTakeBack(context.Background(), 1<<20, "consumer-a")
	if !isBoxFull(err) {
		t.Fatalf("err = %v, want box_full", err)
	}
	if freed != 0 {
		t.Fatalf("freed = %d, want 0", freed)
	}
	// The refused call still closes with its disk.cleanup; only after it
	// is seen is the absence of critical_release meaningful.
	evs.waitFor(t, "disk.cleanup", func(ev LeaseEvent) bool {
		return ev.Type == LeaseDiskCleanup
	})
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
	for _, ev := range evs.all() {
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
	h.tightBox(t, 100<<20, 60, "5")

	// consumer-a asks for more than the box can free: plain-1 is the
	// requester's own lease (never a victim), pinned-1 is unpickable.
	evs := watchEvents(t, h.svc)
	_, err := h.svc.diskTakeBack(context.Background(), 1<<30, "consumer-a")
	var bf *boxFullError
	if !errorsAsBoxFull(err, &bf) {
		t.Fatalf("err = %v, want *boxFullError", err)
	}
	if bf.Owner != "consumer-a" {
		t.Fatalf("boxFullError owner = %q, want the requester", bf.Owner)
	}
	if r := h.leaseRowMust(t, "pinned-1"); !r.Pinned {
		t.Fatalf("pinned-1 = %v, want it intact and pinned", r)
	}
	if r := h.leaseRowMust(t, "plain-1"); r.State != "suspended" {
		t.Fatalf("plain-1 = %v, want it intact", r)
	}
	// The refusal's disk.cleanup proves the stream delivered this call's
	// events before anything is asserted about what never fired.
	evs.waitFor(t, "disk.cleanup", func(ev LeaseEvent) bool {
		return ev.Type == LeaseDiskCleanup
	})
	for _, ev := range evs.all() {
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
	h.tightBox(t, 100<<20, 60, "5")

	if _, err := h.svc.diskTakeBack(context.Background(), 1<<20, "consumer-a"); !isBoxFull(err) {
		t.Fatalf("err = %v, want box_full", err)
	}
	if r := h.leaseRowMust(t, "run-1"); r.State != "running" {
		t.Fatalf("run-1 = %v, want it still running", r)
	}
}

// Statfs lag: after a release the fake's free number stays put, so the
// freeing credit is what tells the loop the need now fits. No second
// deletion happens.
func TestDiskTakeBackStatfsLagNoSecondDeletion(t *testing.T) {
	h := newDiskTBHarness(t, 1<<30)
	h.seedPausedLease(t, "vict-1", "consumer-b", 600<<20, 3*time.Hour, false)
	h.seedPausedLease(t, "vict-2", "consumer-b", 300<<20, 2*time.Hour, false)
	// The box is genuinely tight: 100 MiB free, a 40 MiB running
	// reservation and a 5 % floor leave the 150 MiB need far short.
	h.tightBox(t, 100<<20, 40, "5")

	// The first victim frees 600 MiB; ZFS reports nothing yet (free
	// stays 100 MiB) but the freeing credit covers the 150 MiB need.
	freed, err := h.svc.diskTakeBack(context.Background(), 150<<20, "consumer-a")
	if err != nil {
		t.Fatalf("diskTakeBack: %v", err)
	}
	if freed != 600<<20 {
		t.Fatalf("freed = %d, want only vict-1's %d (lag must not cause a second deletion)", freed, 600<<20)
	}
	if r := h.leaseRowMust(t, "vict-2"); r.State != "suspended" {
		t.Fatalf("vict-2 = %v, want it spared by the freeing credit", r)
	}
	if _, err := h.db.GetLease(context.Background(), "vict-1"); err == nil {
		t.Fatal("vict-1 should have been released")
	}
}

// The loop asks diskVictims for the shortfall, not the full need: with
// 9 GiB usable against a 10 GiB need, a single 2 GiB candidate covers
// the 1 GiB gap and the call succeeds — a selector asked for the whole
// need would call that box full and refuse.
func TestDiskTakeBackShortfallSizing(t *testing.T) {
	h := newDiskTBHarness(t, 1<<30)
	// consumer-b: a running lease whose old 14 GiB pause build still
	// counts for it, plus one suspended 2 GiB lease — the only
	// candidate, and smaller than the 10 GiB need.
	now := time.Now()
	if err := h.db.InsertBuild(context.Background(), store.BuildRow{
		BuildID: "pause-fat", Kind: "pause", ParentBuildID: "fat-run", Owner: "consumer-b",
		State: "ready", SizeBytes: 14 << 30, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("insert fat build: %v", err)
	}
	if err := h.db.UpsertLease(context.Background(), store.LeaseRow{
		ID: "fat-run", Owner: "consumer-b", Image: "py-base", State: "running",
		CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastActive: now,
		ResumeBuildID: "pause-fat", Class: "guaranteed",
	}); err != nil {
		t.Fatalf("insert fat lease: %v", err)
	}
	h.seedPausedLease(t, "vict-1", "consumer-b", 2<<30, 2*time.Hour, false)
	// 9 GiB free, no reservation, no floor: the 10 GiB need is 1 GiB
	// short.
	h.tightBox(t, 9<<30, 0, "0")

	freed, err := h.svc.diskTakeBack(context.Background(), 10<<30, "consumer-a")
	if err != nil {
		t.Fatalf("diskTakeBack: %v", err)
	}
	if freed != 2<<30 {
		t.Fatalf("freed = %d, want the gap-covering victim's %d", freed, 2<<30)
	}
	if _, err := h.db.GetLease(context.Background(), "vict-1"); err == nil {
		t.Fatal("vict-1 should have been released (the shortfall pick)")
	}
	if r := h.leaseRowMust(t, "fat-run"); r.State != "running" {
		t.Fatalf("fat-run = %v, want it intact (the gap was already closed)", r)
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
	h.tightBox(t, 100<<20, 60, "5")

	freed, err := h.svc.diskTakeBack(context.Background(), 1<<20, "consumer-a")
	var bf *boxFullError
	if !errorsAsBoxFull(err, &bf) {
		t.Fatalf("err = %v, want *boxFullError", err)
	}
	if freed != 0 {
		t.Fatalf("freed = %d, want 0", freed)
	}
}

// A stale pick — the candidate list read the catalog, but by the
// moment of commitment the live lease is no longer suspended (a resume
// that landed in between) — releases nothing and emits no
// critical_release: the predicate re-checks the live lease. The loop
// goes around and takes the next candidate instead.
func TestDiskTakeBackStalePickNoEventNoRefusal(t *testing.T) {
	h := newDiskTBHarness(t, 1<<30)
	h.seedPausedLease(t, "vict-1", "consumer-b", 200<<20, time.Hour, false)
	// The loop must go around once: after the stale pick, this second
	// unpinned paused lease of the same owner is the fresh pick.
	h.seedPausedLease(t, "vict-2", "consumer-b", 400<<20, 2*time.Hour, false)
	h.tightBox(t, 100<<20, 0, "5")

	// The resume that wins the race: the in-memory lease is running
	// again while the catalog row still says suspended — exactly the
	// divergence a pick that lands mid-resume sees. The candidate list
	// (from rows) offers oldest vict-1 first; the live predicate must
	// refuse it.
	h.svc.store.mu.Lock()
	if l, ok := h.svc.store.leases["vict-1"]; ok {
		l.Suspended = false
		l.State = "running"
	}
	h.svc.store.mu.Unlock()
	// A live lease emit that fired before the watcher subscribed: the
	// probe below consumes it on its way to disk.cleanup, which is the
	// exact shape a mutated emit-before-release produces (the bogus
	// critical_release is read before the cleanup). Under the committed
	// order the stale pick emits nothing and this extra event is the
	// only noise the waitFor walks past.
	svc := h.svc
	sub := svc.Subscribe(EventFilter{})
	svc.emitLeaseEvent("pre-existing", "legacy-consumer", LeaseCriticalRelease, "pre-existing event before the pass")
	<-sub.C // proof the emit landed on the bus (this watcher's own copy)
	sub.Close()

	evs := watchEvents(t, h.svc)
	freed, err := h.svc.diskTakeBack(context.Background(), 100<<20, "consumer-a")
	if err != nil {
		t.Fatalf("diskTakeBack: %v", err)
	}
	if freed != 400<<20 {
		t.Fatalf("freed = %d, want only vict-2's %d (stale vict-1 pick skipped)", freed, 400<<20)
	}
	// vict-1's row is untouched by the abandoned pick.
	if r := h.leaseRowMust(t, "vict-1"); r.State != "suspended" {
		t.Fatalf("vict-1 = %v, want it intact after its stale pick", r)
	}
	// Wait for the pass's disk.cleanup first: only a delivered stream
	// proves that no critical_release for the stale pick went out.
	evs.waitFor(t, "disk.cleanup", func(ev LeaseEvent) bool {
		return ev.Type == LeaseDiskCleanup
	})
	for _, ev := range evs.all() {
		if ev.Type == LeaseCriticalRelease && ev.LeaseID == "vict-1" {
			t.Fatal("a stale pick must not emit critical_release")
		}
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
	if v := diskVictims(owners, "req", 50, 50); v != nil {
		t.Fatalf("victims = %v, want none (owner would drop to the requester's ratio)", victimIDs(v))
	}
	// A smaller need keeps the owner above after the pick.
	if v := diskVictims(owners, "req", 20, 20); len(v) != 1 || v[0].ID != "a1" {
		t.Fatalf("victims = %v, want a1 (owner stays at 1.3 > 0.7)", v)
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

// containsAll reports whether s contains every substring.
func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

// orphanName returns a UUID-shaped directory name (the reap only
// considers those).
func orphanName(t *testing.T) string {
	t.Helper()
	return uuid.NewString()
}
