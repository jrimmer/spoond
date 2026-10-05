//go:build conformance

package conformance

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

// counterCmd is the group S counter: increments /tmp/ctr every 200 ms in
// the background (exact script from U02).
const counterCmd = "nohup sh -c 'i=0; while :; do i=$((i+1)); echo $i > /tmp/ctr; sleep 0.2; done' >/dev/null 2>&1 & echo started"

// TestS1_SuspendResumeKeepsProcesses suspends a sandbox with a running
// counter and a tmux session and checks that neither advanced through the
// suspended window but both resume.
func TestS1_SuspendResumeKeepsProcesses(t *testing.T) {
	rec := begin(t)
	l := createLease(t, map[string]any{"image": "dev-base", "persistent": true, "ttl": 3600})

	if got := execOK(t, l.ID, counterCmd); got != "started" {
		failf(t, "counter start: got %q", got)
	}
	execOK(t, l.ID, "tmux new-session -d -s conf 'sleep 100000'; tmux ls")

	time.Sleep(2 * time.Second)
	a := readCtr(t, l.ID)

	t0 := time.Now()
	st, body, err := cl.suspend(l.ID)
	if err != nil {
		failf(t, "suspend: %v", err)
	}
	if st != 200 {
		failf(t, "suspend: status %d: %s", st, truncate(body))
	}
	suspendMS := time.Since(t0).Milliseconds()
	rec.set("suspend_ms", suspendMS)

	st, body, err = cl.exec(l.ID, execReq{Cmd: "echo hi"})
	if err != nil {
		failf(t, "exec while suspended: %v", err)
	}
	if st != 409 {
		failf(t, "exec while suspended: status %d, want 409: %s", st, truncate(body))
	}

	time.Sleep(5 * time.Second)

	t1 := time.Now()
	st, body, err = cl.resume(l.ID)
	if err != nil {
		failf(t, "resume: %v", err)
	}
	if st != 200 {
		failf(t, "resume: status %d: %s", st, truncate(body))
	}
	rec.set("resume_ms", time.Since(t1).Milliseconds())

	b := readCtr(t, l.ID)
	time.Sleep(2 * time.Second)
	c := readCtr(t, l.ID)

	maxB := a + int(math.Ceil(float64(suspendMS)/200)) + 10
	if b < a || b > maxB {
		failf(t, "counter through suspend: a=%d b=%d, want a <= b <= a + ceil(suspend_ms/200) + 10 = %d", a, b, maxB)
	}
	if c <= b {
		failf(t, "counter did not advance after resume: b=%d c=%d", b, c)
	}
	if out := execOK(t, l.ID, "tmux ls"); !strings.Contains(out, "conf:") {
		failf(t, "tmux ls = %q, want a conf: session", out)
	}
}

// TestS2_CloneRunningSandbox clones a running sandbox and checks that the
// clone carries the filesystem and processes, and diverges afterwards.
func TestS2_CloneRunningSandbox(t *testing.T) {
	rec := begin(t)
	src := createLease(t, map[string]any{"image": "dev-base", "persistent": true, "ttl": 3600})

	if got := execOK(t, src.ID, counterCmd); got != "started" {
		failf(t, "counter start: got %q", got)
	}
	m := randMarker()
	execOK(t, src.ID, "echo "+m+" > /root/marker")

	t0 := time.Now()
	st, body, err := cl.clone(src.ID, map[string]any{})
	if err != nil {
		failf(t, "clone: %v", err)
	}
	if st != 201 {
		failf(t, "clone: status %d: %s", st, truncate(body))
	}
	rec.set("clone_ms", time.Since(t0).Milliseconds())
	var cloned leaseInfo
	if err := json.Unmarshal(body, &cloned); err != nil || cloned.ID == "" {
		failf(t, "clone: bad body %q: %v", truncate(body), err)
	}
	track(t, cloned.ID)

	if got := execOK(t, cloned.ID, "cat /root/marker"); got != m {
		failf(t, "clone marker: got %q, want %q", got, m)
	}
	c1 := readCtr(t, cloned.ID)
	time.Sleep(1 * time.Second)
	c2 := readCtr(t, cloned.ID)
	if c2 <= c1 {
		failf(t, "clone counter not advancing: %d then %d", c1, c2)
	}
	s1 := readCtr(t, src.ID)
	time.Sleep(1 * time.Second)
	s2 := readCtr(t, src.ID)
	if s2 <= s1 {
		failf(t, "source counter not advancing: %d then %d", s1, s2)
	}

	execOK(t, src.ID, "echo S > /root/marker")
	execOK(t, cloned.ID, "echo C > /root/marker")
	if got := execOK(t, src.ID, "cat /root/marker"); got != "S" {
		failf(t, "source marker after divergence: got %q, want S", got)
	}
	if got := execOK(t, cloned.ID, "cat /root/marker"); got != "C" {
		failf(t, "clone marker after divergence: got %q, want C", got)
	}
}

// TestS3_ForkToEight forks a running sandbox into eight and checks each.
// Requires the U08 fork route.
func TestS3_ForkToEight(t *testing.T) {
	rec := begin(t)
	src := createLease(t, map[string]any{"image": "py-base", "persistent": true, "ttl": 600})

	if got := execOK(t, src.ID, counterCmd); got != "started" {
		failf(t, "counter start: got %q", got)
	}

	t0 := time.Now()
	st, body, err := cl.fork(src.ID, map[string]any{"count": 8})
	if err != nil {
		failf(t, "fork: %v", err)
	}
	if st != 201 {
		failf(t, "fork: status %d, want 201: %s", st, truncate(body))
	}
	rec.set("fork8_ms", time.Since(t0).Milliseconds())

	var fr struct {
		Source  string   `json:"source"`
		BuildID string   `json:"build_id"`
		IDs     []string `json:"ids"`
	}
	if err := json.Unmarshal(body, &fr); err != nil {
		failf(t, "fork: bad body: %v", err)
	}
	if len(fr.IDs) != 8 {
		failf(t, "fork returned %d ids, want 8: %s", len(fr.IDs), truncate(body))
	}
	if fr.Source != src.ID {
		failf(t, "fork source = %q, want %q", fr.Source, src.ID)
	}
	for _, id := range fr.IDs {
		track(t, id)
	}

	// All 8 have an advancing counter.
	for _, id := range fr.IDs {
		r1 := readCtr(t, id)
		time.Sleep(1 * time.Second)
		r2 := readCtr(t, id)
		if r2 <= r1 {
			failf(t, "fork %s counter not advancing: %d then %d", id, r1, r2)
		}
	}
	// Write a distinct marker in each and read each back.
	for _, id := range fr.IDs {
		m := randMarker()
		execOK(t, id, "echo "+m+" > /root/marker")
		if got := execOK(t, id, "cat /root/marker"); got != m {
			failf(t, "fork %s marker: got %q, want %q", id, got, m)
		}
	}
}

// TestS4_CreateLatency measures the create latency of 10 sequential
// py-base create/delete cycles.
func TestS4_CreateLatency(t *testing.T) {
	rec := begin(t)
	var ms []int64
	for range 10 {
		t0 := time.Now()
		l := createLease(t, map[string]any{"image": "py-base", "ttl": 600})
		ms = append(ms, time.Since(t0).Milliseconds())
		st, body, err := cl.delete(l.ID)
		if err != nil {
			failf(t, "delete: %v", err)
		}
		if st != 204 {
			failf(t, "delete: status %d: %s", st, truncate(body))
		}
	}
	sorted := sortedCopy(ms)
	rec.set("create_p50_ms", pct(sorted, 50))
	rec.set("create_p95_ms", pct(sorted, 95))
}

// TestS5_RestartGeneration: generations (2.2, #112) follow the guest's
// memory. Suspend/resume and a persistent restart (a snapshot
// round-trip) continue it: generation 1, and a file in /dev/shm
// survives. A non-persistent restart is a fresh sandbox: generation 2.
// The API and the guest's /run/spoond/generation must agree throughout.
func TestS5_RestartGeneration(t *testing.T) {
	begin(t)

	l := createLease(t, map[string]any{"image": "py-base", "persistent": true, "ttl": 600})
	if g := leaseGeneration(t, l.ID); g != 1 {
		failf(t, "new lease generation %d, want 1", g)
	}
	for _, step := range []struct {
		name string
		do   func(string) (int, []byte, error)
	}{{"suspend", cl.suspend}, {"resume", cl.resume}} {
		st, body, err := step.do(l.ID)
		if err != nil || st != 200 {
			failf(t, "%s: status %d: %v %s", step.name, st, err, truncate(body))
		}
	}
	if g := leaseGeneration(t, l.ID); g != 1 {
		failf(t, "generation %d after suspend/resume, want 1", g)
	}
	execOK(t, l.ID, "echo kept > /dev/shm/s5")
	st, body, err := cl.restart(l.ID)
	if err != nil || st != 200 {
		failf(t, "restart persistent: status %d: %v %s", st, err, truncate(body))
	}
	if out := execOK(t, l.ID, "cat /dev/shm/s5 2>/dev/null || echo gone"); out != "kept" {
		failf(t, "persistent restart lost guest memory: /dev/shm/s5 = %q", out)
	}
	if g := leaseGeneration(t, l.ID); g != 1 {
		failf(t, "generation %d after a persistent restart, want 1 (the memory continued)", g)
	}

	n := createLease(t, map[string]any{"image": "py-base", "ttl": 600})
	st, body, err = cl.restart(n.ID)
	if err != nil || st != 200 {
		failf(t, "restart non-persistent: status %d: %v %s", st, err, truncate(body))
	}
	if g := leaseGeneration(t, n.ID); g != 2 {
		failf(t, "generation %d after a non-persistent restart, want 2", g)
	}
}

// TestS6_RestoreToKeptCheckpoint: a checkpoint with keep is a restore
// point (2.3, #121). A file written after the checkpoint is gone after
// the restore, the lease keeps its id, and the generation bumps to 2 —
// the guest's memory did not continue from where its processes left it.
func TestS6_RestoreToKeptCheckpoint(t *testing.T) {
	rec := begin(t)
	l := createLease(t, map[string]any{"image": "dev-base", "persistent": true, "ttl": 3600})

	m := randMarker()
	execOK(t, l.ID, "echo "+m+" > /root/before-restore")

	t0 := time.Now()
	st, body, err := cl.checkpointKeep(l.ID)
	if err != nil {
		failf(t, "checkpoint keep: %v", err)
	}
	if st != 200 {
		failf(t, "checkpoint keep: status %d: %s", st, truncate(body))
	}
	rec.set("checkpoint_keep_ms", time.Since(t0).Milliseconds())
	var ck struct {
		ID      string `json:"id"`
		BuildID string `json:"build_id"`
		Kept    bool   `json:"kept"`
	}
	if err := json.Unmarshal(body, &ck); err != nil || ck.BuildID == "" {
		failf(t, "checkpoint keep: bad body %q: %v", truncate(body), err)
	}
	if !ck.Kept {
		failf(t, "checkpoint keep: kept = %v, want true", ck.Kept)
	}

	// Work done after the checkpoint.
	execOK(t, l.ID, "echo after > /root/after-restore")

	st, body, err = cl.restore(l.ID, ck.BuildID)
	if err != nil {
		failf(t, "restore: %v", err)
	}
	if st != 200 {
		failf(t, "restore: status %d: %s", st, truncate(body))
	}
	var rs struct {
		ID         string `json:"id"`
		BuildID    string `json:"build_id"`
		Generation int64  `json:"generation"`
	}
	if err := json.Unmarshal(body, &rs); err != nil {
		failf(t, "restore: bad body %q: %v", truncate(body), err)
	}
	if rs.ID != l.ID {
		failf(t, "restore changed the lease id: %q, want %q", rs.ID, l.ID)
	}
	if rs.BuildID != ck.BuildID {
		failf(t, "restore build_id = %q, want the checkpoint build %q", rs.BuildID, ck.BuildID)
	}
	if rs.Generation != 2 {
		failf(t, "restore generation = %d, want 2", rs.Generation)
	}
	if g := leaseGeneration(t, l.ID); g != 2 {
		failf(t, "generation after restore = %d, want 2", g)
	}
	// The post-checkpoint file is gone; the checkpointed state is back.
	if got := execOK(t, l.ID, "cat /root/after-restore 2>/dev/null || echo gone"); got != "gone" {
		failf(t, "post-checkpoint file survived the restore: /root/after-restore = %q", got)
	}
	if got := execOK(t, l.ID, "cat /root/before-restore"); got != m {
		failf(t, "checkpointed file lost in the restore: got %q, want %q", got, m)
	}
}
