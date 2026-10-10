package api

// Request-path disk-room tests (FS3b): create, resume, pause and keep
// (checkpoint {"keep":true}) go through the real HTTP routes on the
// same tight-box harness as the take-back tests. Each test shapes a
// snapshot disk that is a little short, with one reclaimable paused
// lease of another, bigger-borrowing owner: the request releases that
// lease and succeeds. With nothing reclaimable the route answers 429
// with JSON code "box_full".

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v2/store"
	"github.com/jrimmer/spoond/v2/substrate"
)

// diskPathHarness is a diskTBHarness plus the HTTP server over it, so
// the tests drive the real routes as the owner tokens do.
type diskPathHarness struct {
	*diskTBHarness
	ts *httptest.Server
}

func newDiskPathHarness(t *testing.T, free uint64) *diskPathHarness {
	t.Helper()
	h := newDiskTBHarness(t, free)
	seedImage(t, h.db, "py-base", 2048)
	// A huge node: only the snapshot disk is meant to be short here, so
	// the memory admissions never refuse.
	h.sub.SetNodeInfo(substrate.NodeInfo{
		Status:            "healthy",
		HugepagesTotal:    1 << 20,
		HugepageSizeBytes: 2 << 20,
	}, nil)
	// The roomy node replaces the harness's: drop the cached reading so
	// memory admission and the fair slices see it.
	dropNodeCache(h.svc)
	h.svc.updateNodeMetrics(context.Background())
	srv := NewServer(h.svc, NewImageRegistry(h.db))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &diskPathHarness{diskTBHarness: h, ts: ts}
}

// seedSuspendedLease seeds consumer-a's own suspended lease with a
// pause build (its resume needs the root disk back), loads it into the
// store and drops the fair-share cache. The build carries the image's
// template so the resume's createSandbox can start from it.
func (h *diskPathHarness) seedSuspendedLease(t *testing.T, id string) {
	t.Helper()
	ctx := context.Background()
	if err := h.db.UpsertLease(ctx, store.LeaseRow{
		ID: id, Owner: "consumer-a", Image: "py-base", State: "suspended",
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour), LastActive: time.Now(),
		ResumeBuildID: "pause-" + id, Class: "guaranteed", DiskMB: 10240,
		Persistent: true,
	}); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
	build := store.BuildRow{
		BuildID: "pause-" + id, Kind: "pause", ParentBuildID: id,
		Owner: "consumer-a", State: "ready", MemoryMB: 2048, DiskMB: 10240,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if img, err := h.db.GetImage(ctx, "py-base"); err == nil {
		build.TemplateID = img.TemplateID
	}
	if err := h.db.InsertBuild(ctx, build); err != nil {
		t.Fatalf("seed pause build: %v", err)
	}
	if err := h.svc.LoadState(ctx); err != nil {
		t.Fatalf("load state: %v", err)
	}
	h.svc.invalidateFairShares()
}

// seedRunningLeaseAsRow seeds consumer-a's own running lease (a pause
// or a checkpoint writes its sandbox) with the given disk_mb — 0 keeps
// the lease's own reservation out of the room formula — a template
// build to pause/checkpoint from, and a fair-share cache drop.
func (h *diskPathHarness) seedRunningLeaseAsRow(t *testing.T, id, sandboxID string, diskMB int) {
	t.Helper()
	ctx := context.Background()
	buildID := "live-" + id
	build := store.BuildRow{
		BuildID: buildID, Kind: "template", Image: "py-base",
		Owner: "consumer-a", State: "ready", MemoryMB: 2048, DiskMB: diskMB,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if img, err := h.db.GetImage(ctx, "py-base"); err == nil {
		build.TemplateID = img.TemplateID
	}
	if err := h.db.InsertBuild(ctx, build); err != nil {
		t.Fatalf("seed live build: %v", err)
	}
	if err := h.db.UpsertLease(ctx, store.LeaseRow{
		ID: id, Owner: "consumer-a", Image: "py-base", State: "running",
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour), LastActive: time.Now(),
		Class: "guaranteed", DiskMB: diskMB, MemoryMB: 2048, Persistent: true,
		SandboxID: sandboxID,
	}); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
	if sandboxID != "" {
		if err := h.db.UpsertSandbox(ctx, store.SandboxRow{
			SandboxID: sandboxID, LeaseID: id, BuildID: buildID, MemoryMB: 2048, StartedAt: time.Now(),
		}); err != nil {
			t.Fatalf("seed sandbox row: %v", err)
		}
	}
	if err := h.svc.LoadState(ctx); err != nil {
		t.Fatalf("load state: %v", err)
	}
	// LoadState matches the lease's live build through the sandboxes
	// table; a lease with no sandbox row gets the build stamped here.
	h.svc.store.mu.Lock()
	if l := h.svc.store.leases[id]; l != nil && l.BuildID == "" {
		l.BuildID = buildID
	}
	h.svc.store.mu.Unlock()
	h.svc.invalidateFairShares()
}

// expectBoxFull asserts a 429 with JSON code box_full and a bystander
// lease left in the catalog (nothing was released).
func expectBoxFull(t *testing.T, resp *http.Response, body map[string]any, bystanderID string, h *diskPathHarness) {
	t.Helper()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d (%v), want 429 box_full", resp.StatusCode, body)
	}
	if code, _ := body["code"].(string); code != "box_full" {
		t.Fatalf("code = %v, want box_full", body["code"])
	}
	h.leaseRowMust(t, bystanderID)
}

func TestDiskRoomPathCreateTakesBackVictim(t *testing.T) {
	h := newDiskPathHarness(t, 1<<30)
	h.seedFatBorrower(t)
	// 8 GiB free, no floor or reservation: a 10 GiB root disk is 2 GiB
	// short; vict-1's 2 GiB pause build (the oldest) covers it exactly.
	h.tightBox(t, 8<<30, 0, "0")

	resp, body := doReq(t, "POST", h.ts.URL+"/api/sandboxes", "token-a",
		map[string]any{"image": "py-base", "ttl": 300, "persistent": true})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d (%v), want 201 after the take-back", resp.StatusCode, body)
	}
	if _, err := h.db.GetLease(context.Background(), "vict-1"); err == nil {
		t.Fatal("vict-1 should have been released for the create")
	}
	if r := h.leaseRowMust(t, "vict-2"); r.State != "suspended" {
		t.Fatalf("vict-2 = %v, want untouched (the first pick covered the gap)", r)
	}
}

func TestDiskRoomPathCreateNothingReclaimableBoxFull(t *testing.T) {
	h := newDiskPathHarness(t, 1<<30)
	// The only other usage is a kept checkpoint — a kind take-back
	// never touches.
	h.seedNamedAndKept(t, "consumer-b", 0, 900<<20)
	h.tightBox(t, 100<<20, 60, "5")

	resp, body := doReq(t, "POST", h.ts.URL+"/api/sandboxes", "token-a",
		map[string]any{"image": "py-base", "ttl": 300, "persistent": true})
	expectBoxFull(t, resp, body, "lkc-consumer-b", h)
}

func TestDiskRoomPathResumeTakesBackVictim(t *testing.T) {
	h := newDiskPathHarness(t, 1<<30)
	h.seedFatBorrower(t)
	// 9 GiB free: the resumed lease's 10 GiB root disk is 1 GiB short.
	h.tightBox(t, 9<<30, 0, "0")
	h.seedSuspendedLease(t, "a-paused")

	resp, body := doReq(t, "POST", h.ts.URL+"/api/leases/a-paused/resume", "token-a", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("resume: %d (%v), want 200 after the take-back", resp.StatusCode, body)
	}
	if _, err := h.db.GetLease(context.Background(), "vict-1"); err == nil {
		t.Fatal("vict-1 should have been released for the resume")
	}
}

func TestDiskRoomPathResumeNothingReclaimableBoxFull(t *testing.T) {
	h := newDiskPathHarness(t, 1<<30)
	h.seedNamedAndKept(t, "consumer-b", 0, 900<<20)
	h.tightBox(t, 100<<20, 60, "5")
	h.seedSuspendedLease(t, "a-paused")

	resp, body := doReq(t, "POST", h.ts.URL+"/api/leases/a-paused/resume", "token-a", nil)
	expectBoxFull(t, resp, body, "lkc-consumer-b", h)
	if r := h.leaseRowMust(t, "a-paused"); r.State != "suspended" {
		t.Fatalf("a-paused = %v, want it left suspended by the refusal", r)
	}
}

func TestDiskRoomPathPauseTakesBackVictim(t *testing.T) {
	h := newDiskPathHarness(t, 1<<30)
	h.seedFatBorrower(t)
	// 1.5 GiB free, no floor: the pause's 2 GiB snapshot estimate is
	// 0.5 GiB short; vict-1's 2 GiB pause build (the oldest) covers it.
	// The pausing lease itself carries no disk_mb: its own reservation
	// must not double-count against its pause.
	h.tightBox(t, 1536<<20, 0, "0")
	h.seedRunningLeaseAsRow(t, "a-run", "sb-a-run", 0)

	resp, body := doReq(t, "POST", h.ts.URL+"/api/leases/a-run/suspend", "token-a", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("suspend: %d (%v), want 200 after the take-back", resp.StatusCode, body)
	}
	if _, err := h.db.GetLease(context.Background(), "vict-1"); err == nil {
		t.Fatal("vict-1 should have been released for the pause")
	}
	if r := h.leaseRowMust(t, "a-run"); r.State != "suspended" {
		t.Fatalf("a-run = %v, want it suspended after the pause", r)
	}
}

func TestDiskRoomPathPauseNothingReclaimableBoxFull(t *testing.T) {
	h := newDiskPathHarness(t, 1<<30)
	h.seedNamedAndKept(t, "consumer-b", 0, 900<<20)
	h.tightBox(t, 100<<20, 60, "5")
	h.seedRunningLeaseAsRow(t, "a-run", "", 0)

	resp, body := doReq(t, "POST", h.ts.URL+"/api/leases/a-run/suspend", "token-a", nil)
	expectBoxFull(t, resp, body, "lkc-consumer-b", h)
	if r := h.leaseRowMust(t, "a-run"); r.State != "running" {
		t.Fatalf("a-run = %v, want it left running by the refusal", r)
	}
}

func TestDiskRoomPathKeepTakesBackVictim(t *testing.T) {
	h := newDiskPathHarness(t, 1<<30)
	h.seedFatBorrower(t)
	// 1.5 GiB free, no floor: the keep's 2 GiB snapshot estimate is
	// 0.5 GiB short; vict-1's 2 GiB pause build (the oldest) covers it.
	h.tightBox(t, 1536<<20, 0, "0")
	h.seedRunningLeaseAsRow(t, "a-keep", "sb-a-keep", 0)

	resp, body := doReq(t, "POST", h.ts.URL+"/api/leases/a-keep/checkpoint", "token-a",
		map[string]any{"keep": true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("keep: %d (%v), want 200 after the take-back", resp.StatusCode, body)
	}
	if body["kept"] != true {
		t.Fatalf("keep body = %v, want kept true", body)
	}
	if _, err := h.db.GetLease(context.Background(), "vict-1"); err == nil {
		t.Fatal("vict-1 should have been released for the keep")
	}
}

func TestDiskRoomPathKeepNothingReclaimableBoxFull(t *testing.T) {
	h := newDiskPathHarness(t, 1<<30)
	h.seedNamedAndKept(t, "consumer-b", 0, 900<<20)
	h.tightBox(t, 100<<20, 60, "5")
	h.seedRunningLeaseAsRow(t, "a-keep", "", 0)

	resp, body := doReq(t, "POST", h.ts.URL+"/api/leases/a-keep/checkpoint", "token-a",
		map[string]any{"keep": true})
	expectBoxFull(t, resp, body, "lkc-consumer-b", h)
}

// A named save's checkpoint refused for snapshot disk answers like
// every other path: box_full is 429 box_full, a disk still short after
// take-back is 503 capacity_wait with Retry-After — never a 500.
func TestNamedSaveMapsDiskRefusals(t *testing.T) {
	svc, _, _ := newTestService(t)
	bf := svc.mapSnapshotCheckpointError(&boxFullError{Owner: "a"})
	if bf.status != http.StatusTooManyRequests || bf.code != "box_full" {
		t.Fatalf("box_full mapped to %d %s", bf.status, bf.code)
	}
	cw := svc.mapSnapshotCheckpointError(errDiskCapacityWait)
	if cw.status != http.StatusServiceUnavailable || cw.code != "capacity_wait" || cw.retryAfter != capacityRetryAfterSecs {
		t.Fatalf("capacity wait mapped to %d %s retry %d", cw.status, cw.code, cw.retryAfter)
	}
}

// A pause gives back the lease's own disk_mb reservation, so a lease
// whose reservation covers its snapshot pauses even when usable disk is
// negative and nothing can be taken back.
func TestDiskRoomPathPauseCountsItsOwnReservation(t *testing.T) {
	h := newDiskPathHarness(t, 1<<30)
	h.tightBox(t, 100<<20, 0, "5")
	h.seedRunningLeaseAsRow(t, "a-run", "sb-a-run", 4096)

	resp, body := doReq(t, "POST", h.ts.URL+"/api/leases/a-run/suspend", "token-a", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("suspend: %d (%v), want 200: the pause frees its own 4 GiB reservation", resp.StatusCode, body)
	}
	if r := h.leaseRowMust(t, "a-run"); r.State != "suspended" {
		t.Fatalf("a-run = %v, want it suspended", r)
	}
}

// An admin drain is never refused for disk, even when the snapshot is
// bigger than the lease's reservation and nothing can be taken back.
func TestDiskRoomDrainNeverRefusedForDisk(t *testing.T) {
	h := newDiskPathHarness(t, 1<<30)
	h.tightBox(t, 100<<20, 0, "5")
	h.seedRunningLeaseAsRow(t, "a-run", "sb-a-run", 0)

	res, err := h.svc.drain(context.Background())
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	// tightBox's reserve-run has no build to pause; only a-run matters.
	for _, f := range res.Failed {
		if f.ID == "a-run" {
			t.Fatalf("drain failed a-run: %s, want no disk refusal", f.Error)
		}
	}
	if r := h.leaseRowMust(t, "a-run"); r.State != "suspended" {
		t.Fatalf("a-run = %v, want it paused by the drain", r)
	}
}
