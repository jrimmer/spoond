package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jrimmer/spoond/forkd"
)

// newProbeService builds a Service over a fake substrate with one image in
// the pool map, so the warm-pool paths are reachable.
func newProbeService(ff *fakeForkd) *Service {
	return NewService(ff, map[string]string{"t": "c"}, 0, time.Minute, time.Minute, "py-base")
}

// poolSeeded puts a sandbox id in the warm pool for image.
func poolSeeded(svc *Service, image string, ids ...string) {
	svc.store.mu.Lock()
	svc.store.pool[image] = append(svc.store.pool[image], ids...)
	svc.store.mu.Unlock()
}

// A pooled sandbox from a bad image generation answers a ping and then fails
// the job deep inside a build. It must be recycled rather than leased.
func TestGrantRecyclesPooledSandboxThatFailsProbe(t *testing.T) {
	ff := newFakeForkd()
	ff.sandboxes["pooled-bad"] = forkd.SandboxInfo{ID: "pooled-bad", SnapshotTag: "py-base", GuestAddr: "10.42.0.2:8888"}
	ff.probeFail = map[string]string{"pooled-bad": "uname -s -> uniq (GNU coreutils) 9.1"}

	svc := newProbeService(ff)
	poolSeeded(svc, "py-base", "pooled-bad")

	lease, err := svc.grant(context.Background(), "c", "py-base", 0, time.Minute, false, "internet", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if lease.ForkdID == "pooled-bad" {
		t.Fatal("granted the sandbox that failed its integrity probe")
	}
	killed := false
	for _, id := range ff.killed {
		if id == "pooled-bad" {
			killed = true
		}
	}
	if !killed {
		t.Errorf("corrupt pooled sandbox was not recycled; killed=%v", ff.killed)
	}
}

// A cold spawn is probed too: the failure has to be caught before the sandbox
// is handed to a job, and a bad sandbox must not be left running.
func TestGrantRejectsColdSpawnThatFailsProbe(t *testing.T) {
	ff := newFakeForkd()
	ff.probeFailAll = true

	svc := newProbeService(ff)
	_, err := svc.grant(context.Background(), "c", "py-base", 0, time.Minute, false, "internet", nil)
	if err == nil {
		t.Fatal("grant succeeded with a sandbox that failed its integrity probe")
	}
	if len(ff.killed) != 1 {
		t.Errorf("expected the bad sandbox to be killed, killed=%v", ff.killed)
	}
}

// The warm pool must not stock a sandbox that fails the probe, or every grant
// after it inherits the failure.
func TestWarmPoolDoesNotStockSandboxThatFailsProbe(t *testing.T) {
	ff := newFakeForkd()
	ff.probeFailAll = true

	svc := newProbeService(ff)
	svc.poolSize = 1
	svc.warmPool(context.Background(), "py-base")

	svc.store.mu.Lock()
	pooled := len(svc.store.pool["py-base"])
	svc.store.mu.Unlock()
	if pooled != 0 {
		t.Fatalf("pooled %d sandboxes, want 0", pooled)
	}
	if len(ff.killed) != 1 {
		t.Errorf("expected the bad sandbox to be recycled, killed=%v", ff.killed)
	}
}

// A healthy spawn is pooled as before — the probe must not reject good work.
func TestWarmPoolStocksHealthySandbox(t *testing.T) {
	ff := newFakeForkd()
	svc := newProbeService(ff)
	svc.poolSize = 2
	svc.warmPool(context.Background(), "py-base")

	svc.store.mu.Lock()
	pooled := len(svc.store.pool["py-base"])
	svc.store.mu.Unlock()
	if pooled != 2 {
		t.Fatalf("pooled %d sandboxes, want 2; killed=%v", pooled, ff.killed)
	}
}

// SANDBOX_PROBE=0 turns the check off: every sandbox is handed out
// unverified, which is the escape hatch if the probe itself misbehaves.
func TestProbeDisabledGrantsUnverifiedSandbox(t *testing.T) {
	ff := newFakeForkd()
	ff.sandboxes["pooled-bad"] = forkd.SandboxInfo{ID: "pooled-bad", SnapshotTag: "py-base", GuestAddr: "10.42.0.2:8888"}
	ff.probeFail = map[string]string{"pooled-bad": "uname -s -> uniq (GNU coreutils) 9.1"}

	svc := newProbeService(ff)
	svc.SetSandboxProbe(false, 0)
	poolSeeded(svc, "py-base", "pooled-bad")

	lease, err := svc.grant(context.Background(), "c", "py-base", 0, time.Minute, false, "internet", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if lease.ForkdID != "pooled-bad" {
		t.Fatalf("with the probe disabled the pooled sandbox should be granted, got %s", lease.ForkdID)
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
