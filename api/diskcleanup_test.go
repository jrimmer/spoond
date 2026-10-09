package api

// Critical disk and the proactive cleanup tier (#145 D5, owner
// refinement 2026-10-08). Below DISK_CLEAN_START_PCT free, spoond
// reclaims its own garbage before any live work; below
// CRITICAL_DISK_FREE_PCT it releases the oldest suspended lease, one
// per tick, through releaseBecause with reason disk_critical, preceded
// by one critical_release event. A running lease is never released.

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jrimmer/spoond/v2/metrics"
	"github.com/jrimmer/spoond/v2/store"
	"github.com/jrimmer/spoond/v2/substrate/e2b"
)

// seedSizedBuild inserts a ready build row old enough for the GC,
// recording size bytes so the disk.cleanup byte counts are non-zero.
func seedSizedBuild(t *testing.T, db *store.DB, id, kind string, size int64) {
	t.Helper()
	if err := db.InsertBuild(context.Background(), store.BuildRow{
		BuildID: id, Kind: kind, TemplateID: e2b.NewTemplateID(), Image: "py-base",
		Owner: "gone", State: "ready", SizeBytes: size,
		CreatedAt: gcOld, UpdatedAt: gcOld,
	}); err != nil {
		t.Fatalf("seed sized build %s: %v", id, err)
	}
}

// TestCriticalReleaseEndState pins the Honey contract (2026-10-08): a
// critical release goes through releaseBecause like any release. After
// it the lease answers 404 lease not found on every call with no new
// state, and the stream carries one critical_release event immediately
// before the released event whose reason is disk_critical.
func TestCriticalReleaseEndState(t *testing.T) {
	t.Setenv("GC_DELETE", "1")
	ts, svc, db, _ := newTestServerWithService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()
	svc.cfg.CriticalDiskFreePct = 5
	svc.cfg.CriticalDiskRecoverPct = 10
	svc.cfg.TemplateStoragePath = t.TempDir()
	svc.SetMetrics(metrics.NewBackendMetrics())
	svc.diskCapacity = func(string) (uint64, uint64, error) { return 100, 3, nil } // below critical

	events := svc.Subscribe(EventFilter{})

	resp, body := doReq(t, "POST", ts.URL+"/api/leases", "token-a", map[string]any{"image": "py-base", "ttl": 300, "persistent": true})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d (%v), want 201", resp.StatusCode, body)
	}
	id := body["id"].(string)
	l := svc.lookup("consumer-a", id)
	if _, err := svc.pauseLease(ctx, l, false); err != nil {
		t.Fatalf("pause: %v", err)
	}

	svc.runHeldRules(ctx, time.Now())
	if svc.lookup("consumer-a", id) != nil {
		t.Fatal("the suspended lease was not released under critical disk")
	}
	events.Close()
	mine := eventsFor(collectEvents(events.C), id)
	var tail []LeaseEvent
	for _, ev := range mine {
		if ev.Type == LeaseCriticalRelease || ev.Type == LeaseReleased {
			tail = append(tail, ev)
		}
	}
	if len(tail) != 2 || tail[0].Type != LeaseCriticalRelease || tail[1].Type != LeaseReleased {
		t.Fatalf("critical/release events = %v, want [critical_release released]", eventTypes(tail))
	}
	if tail[1].Detail != criticalReleaseReason {
		t.Fatalf("released reason = %q, want %q", tail[1].Detail, criticalReleaseReason)
	}
	if tail[0].Owner != "consumer-a" {
		t.Fatalf("critical_release owner = %q, want consumer-a", tail[0].Owner)
	}

	// Every call now answers 404 lease not found, with no lingering
	// suspended state.
	for _, call := range []struct {
		method, path string
		body         any
	}{
		{"GET", "/api/leases/" + id, nil},
		{"POST", "/api/leases/" + id + "/resume", nil},
		{"DELETE", "/api/leases/" + id, nil},
	} {
		resp, body := doReq(t, call.method, ts.URL+call.path, "token-a", call.body)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s %s = %d (%v), want 404", call.method, call.path, resp.StatusCode, body)
		}
		if body["error"] != "lease not found" {
			t.Fatalf("%s %s error = %v, want lease not found", call.method, call.path, body["error"])
		}
	}
}

// TestDiskCleanupBeforeCriticalFIFO: above CRITICAL_DISK_FREE_PCT the
// proactive tier reclaims spoond's own garbage (an old unreferenced
// pause build left by a released lease) and leaves a live suspended
// lease untouched; below it, the FIFO releases the suspended lease.
// This is the ordering the owner asked for: garbage first, owners'
// suspended leases only as the last resort.
func TestDiskCleanupBeforeCriticalFIFO(t *testing.T) {
	t.Setenv("GC_DELETE", "1")
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()
	svc.SetMetrics(metrics.NewBackendMetrics())
	svc.cfg.CriticalDiskFreePct = 5
	svc.cfg.CriticalDiskRecoverPct = 10
	svc.cfg.DiskCleanStartPct = 20
	svc.cfg.DiskCleanStopPct = 25
	svc.cfg.TemplateStoragePath = t.TempDir()

	l, err := svc.grant(ctx, "c", "py-base", time.Hour, true, "", nil, "ci-job", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := svc.pauseLease(ctx, l, false); err != nil {
		t.Fatalf("pause: %v", err)
	}

	// A leftover checkpoint build with no live lease: only the catalog
	// GC can reclaim it (it is unreferenced, old and sized).
	garbage := e2b.NewUUID()
	seedSizedBuild(t, db, garbage, "checkpoint", 8<<20)

	// 18 % free: below the cleanup start, above critical. The tier runs,
	// the FIFO does not, so the suspended lease survives.
	svc.diskCapacity = func(string) (uint64, uint64, error) { return 100, 18, nil }
	events := svc.Subscribe(EventFilter{})
	svc.runHeldRules(ctx, time.Now())
	events.Close()
	if svc.lookup("c", l.ID) == nil {
		t.Fatal("a live suspended lease was released above the critical level")
	}
	if b, err := db.GetBuild(ctx, garbage); err != nil || b.State != "deleted" {
		t.Fatalf("the leftover build after the cleanup: row=%+v err=%v, want deleted", b, err)
	}
	got := collectEvents(events.C)
	var cleanup *LeaseEvent
	for i := range got {
		if got[i].Type == LeaseDiskCleanup {
			cleanup = &got[i]
		}
	}
	if cleanup == nil {
		t.Fatalf("events = %v, want a disk.cleanup event", eventTypes(got))
	}

	// 3 % free: critical. Now the FIFO releases the suspended lease.
	svc.diskCapacity = func(string) (uint64, uint64, error) { return 100, 3, nil }
	svc.runHeldRules(ctx, time.Now())
	if svc.lookup("c", l.ID) != nil {
		t.Fatal("the suspended lease was not released below the critical level")
	}
}

// TestCriticalNeverReleasesRunningLease: below critical, only a
// suspended lease is a candidate; every running lease survives.
func TestCriticalNeverReleasesRunningLease(t *testing.T) {
	t.Setenv("GC_DELETE", "1")
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()
	svc.SetMetrics(metrics.NewBackendMetrics())
	svc.cfg.CriticalDiskFreePct = 5
	svc.cfg.CriticalDiskRecoverPct = 10
	svc.cfg.TemplateStoragePath = t.TempDir()
	svc.diskCapacity = func(string) (uint64, uint64, error) { return 100, 1, nil } // critical

	a, err := svc.grant(ctx, "c", "py-base", time.Hour, true, "", nil, "running-a", "", nil)
	if err != nil {
		t.Fatalf("grant a: %v", err)
	}
	b, err := svc.grant(ctx, "c", "py-base", time.Hour, true, "", nil, "running-b", "", nil)
	if err != nil {
		t.Fatalf("grant b: %v", err)
	}
	for i := 0; i < 3; i++ {
		svc.runHeldRules(ctx, time.Now().Add(time.Duration(i)*time.Minute))
	}
	if svc.lookup("c", a.ID) == nil || svc.lookup("c", b.ID) == nil {
		t.Fatal("a running lease was released under critical disk")
	}
	if n := heldCounter(t, svc, "critical", "release"); n != 0 {
		t.Fatalf("held_actions_total{critical,release} = %g, want 0 with no suspended lease", n)
	}
}

// TestDiskCleanupExpiresKeptCheckpoint: below DISK_CLEAN_START_PCT a
// kept checkpoint past KEPT_CHECKPOINT_TTL is unpinned so the GC may
// reclaim it in the same tick; the bytes it frees are reported under
// the kept-checkpoint category.
func TestDiskCleanupExpiresKeptCheckpoint(t *testing.T) {
	t.Setenv("GC_DELETE", "1")
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()
	svc.SetMetrics(metrics.NewBackendMetrics())
	svc.cfg.DiskCleanStartPct = 20
	svc.cfg.DiskCleanStopPct = 25
	svc.cfg.KeptCheckpointTTL = time.Hour
	svc.cfg.TemplateStoragePath = t.TempDir()

	l, err := svc.grant(ctx, "c", "py-base", time.Hour, true, "", nil, "ci-job", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	// A kept checkpoint build, old enough that the GC would take it if
	// it were not pinned. Pin it, with an old kept_at.
	build := e2b.NewUUID()
	seedSizedBuild(t, db, build, "checkpoint", 8<<20)
	if err := db.KeepBuild(ctx, l.ID, build, time.Now().Add(-2*time.Hour)); err != nil {
		t.Fatalf("keep: %v", err)
	}

	svc.diskCapacity = func(string) (uint64, uint64, error) { return 100, 10, nil } // below cleanup start
	events := svc.Subscribe(EventFilter{})
	svc.runHeldRules(ctx, time.Now())
	events.Close()
	got := collectEvents(events.C)

	pins, err := db.ListKeptBuildPins(ctx)
	if err != nil {
		t.Fatalf("list pins: %v", err)
	}
	if len(pins) != 0 {
		t.Fatalf("kept pins after the cleanup = %v, want none", pins)
	}
	if b, err := db.GetBuild(ctx, build); err != nil || b.State != "deleted" {
		t.Fatalf("kept build after expiry: row=%+v err=%v, want deleted", b, err)
	}
	var cleanup *LeaseEvent
	for i := range got {
		if got[i].Type == LeaseDiskCleanup {
			cleanup = &got[i]
		}
	}
	if cleanup == nil {
		t.Fatalf("events = %v, want a disk.cleanup event", eventTypes(got))
	}
	if !strings.Contains(cleanup.Detail, "kept checkpoints") {
		t.Fatalf("disk.cleanup detail %q does not name the kept-checkpoint category", cleanup.Detail)
	}
}

// TestDiskCleanupEventNamesCategories: one disk.cleanup event per tick,
// naming the bytes freed per category (orphans, released/lost-lease
// leftovers, kept checkpoints, unreferenced template builds).
func TestDiskCleanupEventNamesCategories(t *testing.T) {
	t.Setenv("GC_DELETE", "1")
	t.Setenv("ORPHAN_REAP", "quarantine")
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()
	svc.SetMetrics(metrics.NewBackendMetrics())
	svc.cfg.DiskCleanStartPct = 20
	svc.cfg.DiskCleanStopPct = 25
	svc.cfg.KeptCheckpointTTL = time.Hour
	svc.cfg.TemplateStoragePath = t.TempDir()

	// An orphan directory (no store row).
	orphanID := uuid.NewString()
	mkOrphanDir(t, svc.cfg.TemplateStoragePath, orphanID)
	ageDir(t, filepath.Join(svc.cfg.TemplateStoragePath, orphanID))

	// A leftover pause build with no live lease (a released lease's
	// build) and an unreferenced template build, both old.
	seedSizedBuild(t, db, e2b.NewUUID(), "pause", 8<<20)
	seedSizedBuild(t, db, e2b.NewUUID(), "template", 8<<20)

	svc.diskCapacity = func(string) (uint64, uint64, error) { return 100, 10, nil }
	events := svc.Subscribe(EventFilter{})
	svc.runHeldRules(ctx, time.Now())
	events.Close()

	var cleanup *LeaseEvent
	got := collectEvents(events.C)
	for i := range got {
		if got[i].Type == LeaseDiskCleanup {
			cleanup = &got[i]
		}
	}
	if cleanup == nil {
		t.Fatalf("events = %v, want a disk.cleanup event", eventTypes(got))
	}
	for _, cat := range []string{"orphans", "released builds", "kept checkpoints", "template builds"} {
		if !strings.Contains(cleanup.Detail, cat) {
			t.Fatalf("disk.cleanup detail %q does not name %q", cleanup.Detail, cat)
		}
	}
}
