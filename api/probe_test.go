package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jrimmer/spoond/store"
	"github.com/jrimmer/spoond/substrate"
)

// newProbeService builds a Service over a fake substrate and temp DB with
// one seeded image, so the warm-pool paths are reachable.
func newProbeService(t *testing.T) (*Service, store.ImageRow, *store.DB, *testSub) {
	t.Helper()
	svc, db, sub := newTestService(t)
	img := seedImage(t, db, "py-base", 2048)
	return svc, img, db, sub
}

// poolSeeded stocks a sandbox in the warm pool for image, with the
// sandboxes row and pool row grant's validation reads.
func poolSeeded(t *testing.T, svc *Service, db *store.DB, sub *testSub, image, buildID string, ids ...string) {
	t.Helper()
	ctx := context.Background()
	for _, id := range ids {
		sb, err := sub.Create(ctx, substrate.CreateRequest{SandboxID: id, BuildID: buildID})
		if err != nil {
			t.Fatalf("seed pooled sandbox: %v", err)
		}
		if err := db.UpsertSandbox(ctx, store.SandboxRow{
			SandboxID: sb.ID, LeaseID: "", BuildID: sb.BuildID,
			ExecutionID: sb.ExecutionID, HostIP: sb.HostIP,
			VCPU: int(sb.VCPU), MemoryMB: int(sb.MemoryMB),
			StartedAt: sb.StartedAt, EndAt: sb.EndAt,
		}); err != nil {
			t.Fatalf("seed sandboxes row: %v", err)
		}
		if err := db.AddPool(ctx, id, image); err != nil {
			t.Fatalf("seed pool row: %v", err)
		}
		svc.store.mu.Lock()
		svc.store.pool[image] = append(svc.store.pool[image], id)
		svc.store.mu.Unlock()
	}
}

// A pooled sandbox from a bad image generation answers a health check and
// then fails the job deep inside a build. The probe must catch it: the
// grant fails and the sandbox is deleted, never leased.
func TestGrantRejectsPooledSandboxThatFailsProbe(t *testing.T) {
	svc, img, db, sub := newProbeService(t)
	svc.cfg.PoolSize = 1
	sub.probeFail["pooled-bad"] = "uname -s -> uniq (GNU coreutils) 9.1"
	poolSeeded(t, svc, db, sub, "py-base", img.CurrentBuildID, "pooled-bad")

	_, err := svc.grant(context.Background(), "c", "py-base", time.Minute, false, "internet", nil)
	if err == nil {
		t.Fatal("grant succeeded with a sandbox that failed its integrity probe")
	}
	if got := calls(sub.Fake, "Delete pooled-bad"); got != 1 {
		t.Errorf("corrupt pooled sandbox was not deleted; calls=%v", sub.Fake.CallLog())
	}
}

// A cold create is probed too: the failure has to be caught before the
// sandbox is handed to a job, and a bad sandbox must not be left running.
func TestGrantRejectsColdCreateThatFailsProbe(t *testing.T) {
	svc, _, _, sub := newProbeService(t)
	sub.probeFailAll = true

	_, err := svc.grant(context.Background(), "c", "py-base", time.Minute, false, "internet", nil)
	if err == nil {
		t.Fatal("grant succeeded with a sandbox that failed its integrity probe")
	}
	if got := calls(sub.Fake, "Delete"); got != 1 {
		t.Errorf("expected the bad sandbox to be deleted, deletes=%d", got)
	}
}

// The warm pool must not stock a sandbox that fails the probe, or every grant
// after it inherits the failure.
func TestWarmPoolDoesNotStockSandboxThatFailsProbe(t *testing.T) {
	svc, img, _, sub := newProbeService(t)
	sub.probeFailAll = true

	svc.cfg.PoolSize = 1
	svc.warmPool(context.Background(), img)

	svc.store.mu.Lock()
	pooled := len(svc.store.pool["py-base"])
	svc.store.mu.Unlock()
	if pooled != 0 {
		t.Fatalf("pooled %d sandboxes, want 0", pooled)
	}
	if got := calls(sub.Fake, "Delete"); got != 1 {
		t.Errorf("expected the bad sandbox to be recycled, deletes=%d", got)
	}
}

// A healthy create is pooled as before — the probe must not reject good work.
func TestWarmPoolStocksHealthySandbox(t *testing.T) {
	svc, img, _, sub := newProbeService(t)
	svc.cfg.PoolSize = 2
	svc.warmPool(context.Background(), img)

	svc.store.mu.Lock()
	pooled := len(svc.store.pool["py-base"])
	svc.store.mu.Unlock()
	if pooled != 2 {
		t.Fatalf("pooled %d sandboxes, want 2; calls=%v", pooled, sub.Fake.CallLog())
	}
}

// SANDBOX_PROBE=0 turns the check off: every sandbox is handed out
// unverified, which is the escape hatch if the probe itself misbehaves.
func TestProbeDisabledGrantsUnverifiedSandbox(t *testing.T) {
	svc, img, db, sub := newProbeService(t)
	svc.cfg.PoolSize = 1
	sub.probeFail["pooled-bad"] = "uname -s -> uniq (GNU coreutils) 9.1"
	poolSeeded(t, svc, db, sub, "py-base", img.CurrentBuildID, "pooled-bad")

	svc.SetSandboxProbe(false, 0)
	lease, err := svc.grant(context.Background(), "c", "py-base", time.Minute, false, "internet", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if lease.SandboxID != "pooled-bad" {
		t.Fatalf("with the probe disabled the pooled sandbox should be granted, got %s", lease.SandboxID)
	}
}

// The probe script must not be fooled by a binary that merely runs. A swapped
// uname is a valid ELF that exits 0 while behaving like another program, which
// is exactly the case an exit-status check passes.
func TestIntegrityProbeChecksBehaviourNotExitStatus(t *testing.T) {
	for _, want := range []string{"uname -s", "tr", "PROBE_OK"} {
		if !strings.Contains(integrityProbe, want) {
			t.Errorf("probe script does not exercise %q:\n%s", want, integrityProbe)
		}
	}
	if strings.Contains(integrityProbe, "--version") {
		t.Error("probe relies on --version, which non-GNU tools answer differently; check behaviour instead")
	}
}

// Only a lease actually handed out counts toward the image's lifetime
// uses; a grant that fails its probe does not.
func TestGrantCountsImageUse(t *testing.T) {
	svc, _, db, sub := newProbeService(t)
	ctx := context.Background()
	if _, err := svc.grant(ctx, "c", "py-base", time.Minute, false, "internet", nil); err != nil {
		t.Fatalf("grant: %v", err)
	}
	sub.probeFailAll = true
	if _, err := svc.grant(ctx, "c", "py-base", time.Minute, false, "internet", nil); err == nil {
		t.Fatal("grant succeeded with a failing probe")
	}
	uses, err := db.ImageUses(ctx)
	if err != nil {
		t.Fatalf("uses: %v", err)
	}
	if uses["py-base"] != 1 {
		t.Fatalf("py-base uses = %d, want 1", uses["py-base"])
	}
}
