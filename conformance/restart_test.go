//go:build conformance

package conformance

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
)

// requireDestructive gates group R: it only runs with
// CONFORMANCE_DESTRUCTIVE=1, under the Autonomous window protocol.
func requireDestructive(t *testing.T) {
	if !cfg.Destructive {
		skipf(t, "group R requires CONFORMANCE_DESTRUCTIVE=1")
	}
}

// requireE2B skips substrate-specific tests on forkd.
func requireE2B(t *testing.T) {
	if cfg.Substrate != "e2b" {
		skipf(t, "e2b only (substrate %q)", cfg.Substrate)
	}
}

// TestR1_PlannedRestart drains 5 leases through an orchestrator restart
// and requires every one of them back with state intact.
func TestR1_PlannedRestart(t *testing.T) {
	rec := begin(t)
	requireDestructive(t)
	requireE2B(t)

	ids := make([]string, 5)
	for i := range 5 {
		l := createLease(t, map[string]any{"image": "dev-base", "persistent": true, "ttl": 3600})
		ids[i] = l.ID
		if got := execOK(t, l.ID, counterCmd); got != "started" {
			failf(t, "lease %d counter start: got %q", i, got)
		}
		execOK(t, l.ID, "tmux new-session -d -s conf-"+strconv.Itoa(i)+" 'sleep 100000'")
	}

	t0 := time.Now()
	hostRun(t, "systemctl restart e2b-orchestrator")

	deadline := time.Now().Add(180 * time.Second)
	for {
		all := true
		for _, id := range ids {
			if !execAlive(id) {
				all = false
				break
			}
		}
		if all {
			break
		}
		if time.Now().After(deadline) {
			failf(t, "leases not all alive 180s after orchestrator restart")
		}
		time.Sleep(2 * time.Second)
	}
	rec.set("restart_total_ms", time.Since(t0).Milliseconds())

	for i, id := range ids {
		r1 := readCtr(t, id)
		time.Sleep(1 * time.Second)
		r2 := readCtr(t, id)
		if r2 <= r1 {
			failf(t, "lease %s counter not advancing after restart: %d then %d", id, r1, r2)
		}
		if out := execOK(t, id, "tmux ls"); !strings.Contains(out, "conf-"+strconv.Itoa(i)) {
			failf(t, "lease %s: tmux ls = %q, want conf-%d session", id, out, i)
		}
	}
}

// TestR2_OrchestratorCrash kills the orchestrator and requires every
// checkpointed lease to come back recovered, from its checkpoint state.
func TestR2_OrchestratorCrash(t *testing.T) {
	begin(t)
	requireDestructive(t)
	requireE2B(t)

	ids := make([]string, 3)
	for i := range 3 {
		l := createLease(t, map[string]any{"image": "py-base", "persistent": true, "ttl": 3600})
		ids[i] = l.ID
		st, body, err := cl.checkpoint(l.ID)
		if err != nil {
			failf(t, "checkpoint: %v", err)
		}
		if st != 200 {
			failf(t, "checkpoint: status %d: %s", st, truncate(body))
		}
		execOK(t, l.ID, "echo after > /root/m")
	}

	n0, err := strconv.Atoi(hostRun(t, "ip netns list | grep -c '^ns-' || true"))
	if err != nil {
		failf(t, "netns count: %v", err)
	}
	hostRun(t, "systemctl kill -s SIGKILL e2b-orchestrator")

	deadline := time.Now().Add(180 * time.Second)
	recovered := map[string]bool{}
	for len(recovered) < len(ids) {
		for _, id := range ids {
			if recovered[id] {
				continue
			}
			st, body, err := cl.do("GET", "/api/sandboxes/"+id, nil)
			if err != nil || st != 200 {
				continue
			}
			var l leaseInfo
			if err := json.Unmarshal(body, &l); err != nil {
				continue
			}
			if l.State == "recovered" {
				recovered[id] = true
			}
		}
		if len(recovered) == len(ids) {
			break
		}
		if time.Now().After(deadline) {
			failf(t, "leases not all recovered 180s after the crash: recovered %v", recovered)
		}
		time.Sleep(2 * time.Second)
	}

	for _, id := range ids {
		if out := execOK(t, id, "test -e /root/m && echo present || echo absent"); out != "absent" {
			failf(t, "lease %s: /root/m = %q, want absent (state is from the checkpoint)", id, out)
		}
		if out := execOK(t, id, "echo alive"); out != "alive" {
			failf(t, "lease %s: echo alive: got %q", id, out)
		}
	}

	// One NBD rootfs per running sandbox.
	nbd, err := strconv.Atoi(hostRun(t, "ls -d /sys/block/nbd*/pid 2>/dev/null | wc -l"))
	if err != nil {
		failf(t, "nbd count: %v", err)
	}
	fc, err := strconv.Atoi(hostRun(t, "pgrep -f '^/fc-versions/' | wc -l"))
	if err != nil {
		failf(t, "firecracker process count: %v", err)
	}
	if nbd != fc {
		failf(t, "nbd devices = %d, firecracker processes = %d, want equal", nbd, fc)
	}
	ns, err := strconv.Atoi(hostRun(t, "ip netns list | grep -c '^ns-' || true"))
	if err != nil {
		failf(t, "netns count: %v", err)
	}
	if ns != n0 {
		failf(t, "netns count = %d, want %d (as before the crash)", ns, n0)
	}
}

// TestR3_BackendRestart restarts the backend under test and requires the
// leases to survive it.
func TestR3_BackendRestart(t *testing.T) {
	begin(t)
	requireDestructive(t)

	ids := make([]string, 2)
	for i := range 2 {
		l := createLease(t, map[string]any{"image": "py-base", "persistent": true, "ttl": 3600})
		ids[i] = l.ID
	}

	hostRun(t, "systemctl restart "+cfg.BackendUnit)

	deadline := time.Now().Add(60 * time.Second)
	for {
		st, _, err := cl.do("GET", "/healthz", nil)
		if err == nil && st == 200 {
			break
		}
		if time.Now().After(deadline) {
			failf(t, "backend not healthy 60s after restart")
		}
		time.Sleep(1 * time.Second)
	}

	for _, id := range ids {
		if out := execOK(t, id, "echo alive"); out != "alive" {
			failf(t, "lease %s after backend restart: echo alive: got %q", id, out)
		}
	}
}
