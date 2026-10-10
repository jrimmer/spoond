package api

// ensureDiskRoom tests (FS3b-1): the helper alone, on the same tight-box
// harness as the take-back tests. Request call sites are wired later.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v2/store"
	"github.com/jrimmer/spoond/v2/substrate"
)

// seedFatBorrower makes consumer-b the biggest borrower: a running lease
// whose old 14 GiB pause build still counts for it, plus one suspended
// 2 GiB lease (vict-1, older) and a newer 1 GiB one (vict-2).
func (h *diskTBHarness) seedFatBorrower(t *testing.T) {
	t.Helper()
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
	h.seedPausedLease(t, "vict-2", "consumer-b", 1<<30, time.Hour, false)
}

func (h *diskTBHarness) pending() int64 {
	p, _ := h.svc.diskInflight.settle(time.Now(), int64(h.free))
	return p
}

func TestEnsureDiskRoomFitsNoTakeBack(t *testing.T) {
	h := newDiskTBHarness(t, 1<<30)
	h.seedFatBorrower(t)
	h.tightBox(t, 9<<30, 0, "0")

	release, err := h.svc.ensureDiskRoom(context.Background(), "consumer-a", 1<<30)
	if err != nil {
		t.Fatalf("ensureDiskRoom: %v", err)
	}
	if got := h.pending(); got != 1<<30 {
		t.Fatalf("pending = %d, want the reserved %d", got, int64(1<<30))
	}
	for _, id := range []string{"vict-1", "vict-2"} {
		if r := h.leaseRowMust(t, id); r.State != "suspended" {
			t.Fatalf("%s = %v, want untouched (the need fits)", id, r)
		}
	}
	release()
	if got := h.pending(); got != 0 {
		t.Fatalf("pending after release = %d, want 0", got)
	}
}

func TestEnsureDiskRoomTakesBackOldestThenSucceeds(t *testing.T) {
	h := newDiskTBHarness(t, 1<<30)
	h.seedFatBorrower(t)
	// 9 GiB free, no floor or reservation: a 10 GiB need is 1 GiB short.
	h.tightBox(t, 9<<30, 0, "0")

	release, err := h.svc.ensureDiskRoom(context.Background(), "consumer-a", 10<<30)
	if err != nil {
		t.Fatalf("ensureDiskRoom: %v", err)
	}
	defer release()
	if _, err := h.db.GetLease(context.Background(), "vict-1"); err == nil {
		t.Fatal("vict-1 (oldest unpinned paused lease of the biggest borrower) should be released")
	}
	if r := h.leaseRowMust(t, "vict-2"); r.State != "suspended" {
		t.Fatalf("vict-2 = %v, want untouched (the first pick covered the gap)", r)
	}
	if got := h.pending(); got != 10<<30 {
		t.Fatalf("pending = %d, want %d", got, int64(10<<30))
	}
}

func TestEnsureDiskRoomNothingReclaimableBoxFull(t *testing.T) {
	h := newDiskTBHarness(t, 1<<30)
	h.seedNamedAndKept(t, "consumer-b", 0, 900<<20)
	h.tightBox(t, 100<<20, 60, "5")

	release, err := h.svc.ensureDiskRoom(context.Background(), "consumer-a", 1<<20)
	var bf *boxFullError
	if !errors.As(err, &bf) {
		t.Fatalf("err = %v, want *boxFullError", err)
	}
	if release != nil {
		t.Fatal("a refusal must not return a release")
	}
	if !isBoxFull(err) {
		t.Fatal("the refusal must match errBoxFull for the 429 mapping")
	}
	if got := h.pending(); got != 0 {
		t.Fatalf("pending = %d after a refusal, want 0", got)
	}
}

func TestEnsureDiskRoomNoDiskInfoAllows(t *testing.T) {
	h := newDiskTBHarness(t, 1<<30)
	h.svc.cfg.TemplateStoragePath = ""

	release, err := h.svc.ensureDiskRoom(context.Background(), "consumer-a", 1<<40)
	if err != nil {
		t.Fatalf("ensureDiskRoom without disk info: %v", err)
	}
	release()
	if got := h.pending(); got != 0 {
		t.Fatalf("pending = %d, want 0 (nothing reserved without info)", got)
	}
}

func TestEnsureDiskRoomNonPositiveNeedIsNoop(t *testing.T) {
	h := newDiskTBHarness(t, 1<<30)
	release, err := h.svc.ensureDiskRoom(context.Background(), "consumer-a", 0)
	if err != nil || release == nil {
		t.Fatalf("ensureDiskRoom(0) = %v, %v; want a no-op release", release != nil, err)
	}
	release()
}

func TestEnsureDiskRoomStillShortIsCapacityWait(t *testing.T) {
	h := newDiskTBHarness(t, 1<<30)
	h.seedFatBorrower(t)
	// A need far beyond the victims' 3 GiB, but a victim exists, so the
	// selector keeps going until it runs out of candidates.
	h.tightBox(t, 9<<30, 0, "0")
	_, err := h.svc.ensureDiskRoom(context.Background(), "consumer-a", 100<<30)
	if err == nil {
		t.Fatal("want a refusal for a need no take-back can cover")
	}
	if !errors.Is(err, substrate.ErrCapacity) && !isBoxFull(err) {
		t.Fatalf("err = %v, want capacity_wait (substrate.ErrCapacity) or box_full", err)
	}
}

func TestEnsureDiskRoomReleaseIdempotent(t *testing.T) {
	h := newDiskTBHarness(t, 1<<30)
	h.tightBox(t, 9<<30, 0, "0")

	r1, err := h.svc.ensureDiskRoom(context.Background(), "consumer-a", 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := h.svc.ensureDiskRoom(context.Background(), "consumer-a", 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	if got := h.pending(); got != 2<<30 {
		t.Fatalf("pending = %d, want %d", got, int64(2<<30))
	}
	r1()
	r1()
	if got := h.pending(); got != 1<<30 {
		t.Fatalf("pending after double release = %d, want %d (the second reservation stays)", got, int64(1<<30))
	}
	r2()
	if got := h.pending(); got != 0 {
		t.Fatalf("pending = %d, want 0", got)
	}
}
