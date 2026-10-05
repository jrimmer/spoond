//go:build conformance

package conformance

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestL1_CreateExecEachImage creates, execs in and deletes every image in
// CONFORMANCE_IMAGES.
func TestL1_CreateExecEachImage(t *testing.T) {
	rec := begin(t)
	for _, img := range cfg.Images {
		t0 := time.Now()
		l := createLease(t, map[string]any{"image": img, "ttl": 600})
		rec.set("create_ms:"+img, time.Since(t0).Milliseconds())
		if got := execOK(t, l.ID, "echo ok"); got != "ok" {
			failf(t, "%s: echo ok: got %q", img, got)
		}
		st, body, err := cl.exec(l.ID, execReq{Cmd: "echo err >&2; exit 3"})
		if err != nil {
			failf(t, "%s: exec: %v", img, err)
		}
		if st != 200 {
			failf(t, "%s: exec: status %d: %s", img, st, truncate(body))
		}
		var out execResult
		if err := json.Unmarshal(body, &out); err != nil {
			failf(t, "%s: exec: bad body: %v", img, err)
		}
		if out.Exit != 3 {
			failf(t, "%s: exec exit = %d, want 3 (stderr %q)", img, out.Exit, out.Stderr)
		}
		if out.Stderr != "err\n" {
			failf(t, "%s: exec stderr = %q, want %q", img, out.Stderr, "err\n")
		}
		st, body, err = cl.delete(l.ID)
		if err != nil {
			failf(t, "%s: delete: %v", img, err)
		}
		if st != 204 {
			failf(t, "%s: delete: status %d: %s", img, st, truncate(body))
		}
	}
}

// TestL2_ExecTimeout caps a sleeping exec at 2 s and expects a non-zero
// exit within 10 s; the lease must stay usable afterwards.
func TestL2_ExecTimeout(t *testing.T) {
	begin(t)
	l := createLease(t, map[string]any{"image": "py-base", "ttl": 600})
	t0 := time.Now()
	st, body, err := cl.exec(l.ID, execReq{Cmd: "sleep 30", Timeout: 2})
	if err != nil {
		failf(t, "exec: %v", err)
	}
	if d := time.Since(t0); d > 10*time.Second {
		failf(t, "timed-out exec took %s, want < 10s", d)
	}
	if st != 200 {
		failf(t, "exec status %d: %s", st, truncate(body))
	}
	var out execResult
	if err := json.Unmarshal(body, &out); err != nil {
		failf(t, "exec bad body: %v", err)
	}
	if out.Exit == 0 {
		failf(t, "timed-out exec exit = 0, want non-zero")
	}
	if got := execOK(t, l.ID, "echo alive"); got != "alive" {
		failf(t, "echo alive: got %q", got)
	}
}

// TestL3_StreamPTY opens a PTY stream, runs a command and exits the shell.
func TestL3_StreamPTY(t *testing.T) {
	begin(t)
	l := createLease(t, map[string]any{"image": "py-base", "ttl": 600})
	ws, err := cl.stream(l.ID, streamReq{Args: []string{"/bin/bash", "-l"}, Pty: true})
	if err != nil {
		failf(t, "stream: %v", err)
	}
	defer ws.close()

	// Started frame within 10 s.
	deadline := time.Now().Add(10 * time.Second)
	for {
		f, err := ws.read(time.Until(deadline))
		if err != nil {
			failf(t, "no stream started frame within 10s: %v", err)
		}
		if _, ok := f["stream"]; ok {
			break
		}
	}

	if err := ws.send(map[string]any{"in": "echo MARK$((40+2))\n"}); err != nil {
		failf(t, "send in: %v", err)
	}
	deadline = time.Now().Add(10 * time.Second)
	found := false
	for !found {
		f, err := ws.read(time.Until(deadline))
		if err != nil {
			failf(t, "no MARK42 output within 10s: %v", err)
		}
		if out, ok := f["out"].(string); ok && strings.Contains(out, "MARK42") {
			found = true
		}
	}

	if err := ws.send(map[string]any{"in": "exit 7\n"}); err != nil {
		failf(t, "send exit: %v", err)
	}
	deadline = time.Now().Add(10 * time.Second)
	for {
		f, err := ws.read(time.Until(deadline))
		if err != nil {
			failf(t, "no exit_code frame within 10s: %v", err)
		}
		if ec, ok := f["exit_code"]; ok {
			if ec != float64(7) {
				failf(t, "exit_code = %v, want 7", ec)
			}
			return
		}
	}
}

// TestL4_StreamNoPTY runs a command over a non-PTY stream: concatenated
// stdout is exactly "A\nB\n" and the exit code is 0.
func TestL4_StreamNoPTY(t *testing.T) {
	begin(t)
	l := createLease(t, map[string]any{"image": "py-base", "ttl": 600})
	ws, err := cl.stream(l.ID, streamReq{
		Args: []string{"/bin/sh", "-c", "echo A; echo B"},
		Pty:  false,
	})
	if err != nil {
		failf(t, "stream: %v", err)
	}
	defer ws.close()

	var sb strings.Builder
	for {
		f, err := ws.read(10 * time.Second)
		if err != nil {
			failf(t, "stream ended without exit_code: %v (out %q)", err, sb.String())
		}
		if out, ok := f["out"].(string); ok {
			sb.WriteString(out)
		}
		if ec, ok := f["exit_code"]; ok {
			if ec != float64(0) {
				failf(t, "exit_code = %v, want 0", ec)
			}
			break
		}
	}
	if got := sb.String(); got != "A\nB\n" {
		failf(t, "concatenated out = %q, want %q", got, "A\nB\n")
	}
}

// TestL5_KeepaliveAndTTL exercises the TTL sweeper and persistent-lease
// keepalive.
func TestL5_KeepaliveAndTTL(t *testing.T) {
	begin(t)
	l1 := createLease(t, map[string]any{"image": "py-base", "ttl": 20})

	time.Sleep(30 * time.Second)
	st, body, err := cl.exec(l1.ID, execReq{Cmd: "echo x"})
	if err != nil {
		failf(t, "exec expired: %v", err)
	}
	if st != 404 {
		failf(t, "exec expired lease: status %d, want 404 (the TTL sweeper released it): %s", st, truncate(body))
	}

	created := time.Now()
	l2 := createLease(t, map[string]any{"image": "py-base", "ttl": 20, "persistent": true})
	st, body, err = cl.keepalive(l2.ID, map[string]any{"ttl": 120})
	if err != nil {
		failf(t, "keepalive: %v", err)
	}
	if st != 200 {
		failf(t, "keepalive: status %d: %s", st, truncate(body))
	}
	var ka struct {
		ID         string `json:"id"`
		Persistent bool   `json:"persistent"`
		ExpiresAt  string `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &ka); err != nil {
		failf(t, "keepalive: bad body: %v", err)
	}
	if ka.ID != l2.ID || !ka.Persistent {
		failf(t, "keepalive body: id %q persistent %v, want id %q persistent true", ka.ID, ka.Persistent, l2.ID)
	}
	exp, err := time.Parse(time.RFC3339, ka.ExpiresAt)
	if err != nil {
		failf(t, "keepalive: bad expires_at %q: %v", ka.ExpiresAt, err)
	}
	if until := time.Until(exp); until < 110*time.Second {
		failf(t, "expires_at %s is only %s away, want >= 110s", ka.ExpiresAt, until)
	}

	if d := 30*time.Second - time.Since(created); d > 0 {
		time.Sleep(d)
	}
	if got := execOK(t, l2.ID, "echo alive"); got != "alive" {
		failf(t, "persistent lease at 30s: echo alive: got %q", got)
	}
}

// TestL6_StatAndHealth checks stat, /healthz and /metrics.
func TestL6_StatAndHealth(t *testing.T) {
	rec := begin(t)
	l := createLease(t, map[string]any{"image": "py-base", "ttl": 600})

	st, body, err := cl.stat(l.ID)
	if err != nil {
		failf(t, "stat: %v", err)
	}
	if st != 200 {
		failf(t, "stat: status %d: %s", st, truncate(body))
	}
	var sr statResult
	if err := json.Unmarshal(body, &sr); err != nil {
		failf(t, "stat: bad body: %v", err)
	}
	if sr.Mem.TotalMiB <= 0 {
		failf(t, "stat: mem.total_mib = %d, want > 0", sr.Mem.TotalMiB)
	}
	if sr.Disk.TotalMiB <= 0 {
		failf(t, "stat: disk.total_mib = %d, want > 0", sr.Disk.TotalMiB)
	}

	st, body, err = cl.do("GET", "/healthz", nil)
	if err != nil {
		failf(t, "healthz: %v", err)
	}
	if st != 200 {
		failf(t, "healthz: status %d: %s", st, truncate(body))
	}

	st, body, err = cl.do("GET", "/metrics", nil)
	if err != nil {
		failf(t, "metrics: %v", err)
	}
	rec.set("metrics_status", st)
	switch st {
	case 200:
		if !strings.Contains(string(body), "spoond_leases_active") {
			failf(t, "metrics body lacks spoond_leases_active")
		}
	case 403:
		if cfg.BackendUnit == "spoond-backend-staging" {
			failf(t, "metrics: 403 on staging, where the conformance user is admin")
		}
	default:
		failf(t, "metrics: status %d, want 200 or 403: %s", st, truncate(body))
	}
}

// TestL7_NoWorldWritableSystemBinaries (#124): nothing under /usr is
// writable by group or others, and envd (it runs as root) is 0755. E2B's
// template build leaves /usr/local 777 and envd 0777; spoond-guest-init
// tightens them before the template is snapshotted.
func TestL7_NoWorldWritableSystemBinaries(t *testing.T) {
	begin(t)
	l := createLease(t, map[string]any{"image": "py-base", "ttl": 300})
	out := execOK(t, l.ID, "find /usr -xdev \\( -type f -o -type d \\) -perm /022 -not -type l 2>/dev/null | head -5; stat -c '%a' /usr/bin/envd")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if mode := lines[len(lines)-1]; mode != "755" {
		failf(t, "/usr/bin/envd mode %s, want 755", mode)
	}
	if len(lines) > 1 {
		failf(t, "group/world-writable paths under /usr:\n%s", strings.Join(lines[:len(lines)-1], "\n"))
	}
}
