package api

import (
	"errors"
	"testing"
	"time"
)

// readGeneration reads /run/spoond/generation out of the fake sandbox.
// Fails the test when the file does not exist.
func readGeneration(t *testing.T, sub *testSub, sandboxID string) string {
	t.Helper()
	data, err := sub.Fake.ReadFile(t.Context(), sandboxID, generationPath, 1024)
	if err != nil {
		t.Fatalf("read %s from %s: %v", generationPath, sandboxID, err)
	}
	return string(data)
}

// TestGenerationCreate: a fresh lease is on generation 1 and the guest
// carries the file /run/spoond/generation with it.
func TestGenerationCreate(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := t.Context()

	l, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "", "")
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if l.Generation != 1 {
		t.Fatalf("generation = %d, want 1", l.Generation)
	}
	if got := readGeneration(t, sub, l.SandboxID); got != "1\n" {
		t.Fatalf("guest file = %q, want \"1\\n\"", got)
	}
	info, err := sub.Fake.Stat(ctx, l.SandboxID, generationPath)
	if err != nil {
		t.Fatalf("stat %s: %v", generationPath, err)
	}
	if info.Mode.Perm() != 0o644 {
		t.Fatalf("file mode = %v, want 0644", info.Mode.Perm())
	}
	dirInfo, err := sub.Fake.Stat(ctx, l.SandboxID, "/run/spoond")
	if err != nil {
		t.Fatalf("stat /run/spoond: %v", err)
	}
	if !dirInfo.IsDir || dirInfo.Mode.Perm() != 0o755 {
		t.Fatalf("dir mode = %v dir=%v, want 0755", dirInfo.Mode.Perm(), dirInfo.IsDir)
	}

	// A pooled sandbox handed to a lease gets the file with the lease's
	// generation too (the pool placeholder's write is overwritten).
	svc.cfg.PoolSize = 1
	svc.refillPool(ctx)
	pooled, err := svc.grant(ctx, "c", "py-base", time.Minute, false, "", nil, "", "")
	if err != nil {
		t.Fatalf("grant pooled: %v", err)
	}
	if !pooled.pooled {
		t.Fatal("second grant should have come from the pool")
	}
	if pooled.Generation != 1 {
		t.Fatalf("pooled generation = %d, want 1", pooled.Generation)
	}
	if got := readGeneration(t, sub, pooled.SandboxID); got != "1\n" {
		t.Fatalf("pooled guest file = %q, want \"1\\n\"", got)
	}
}

// TestGenerationRestart: both restart paths bump the generation and
// rewrite the guest file. Persistent: suspend then resume. Non-
// persistent: a fresh sandbox from the current build. A planned
// suspend/resume in between must not bump on its own.
func TestGenerationRestart(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := t.Context()

	p, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "", "")
	if err != nil {
		t.Fatalf("grant persistent: %v", err)
	}
	n, err := svc.grant(ctx, "c", "py-base", time.Minute, false, "", nil, "", "")
	if err != nil {
		t.Fatalf("grant non-persistent: %v", err)
	}

	// A planned pause/resume continues the memory: no bump.
	if _, err := svc.suspend(ctx, "c", p.ID); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if _, err := svc.resume(ctx, "c", p.ID); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if p.Generation != 1 {
		t.Fatalf("generation after suspend/resume = %d, want 1", p.Generation)
	}
	if got := readGeneration(t, sub, p.SandboxID); got != "1\n" {
		t.Fatalf("guest file after suspend/resume = %q, want \"1\\n\"", got)
	}

	if _, err := svc.restart(ctx, "c", p.ID); err != nil {
		t.Fatalf("restart persistent: %v", err)
	}
	if p.Generation != 2 {
		t.Fatalf("generation after persistent restart = %d, want 2", p.Generation)
	}
	if got := readGeneration(t, sub, p.SandboxID); got != "2\n" {
		t.Fatalf("guest file after persistent restart = %q, want \"2\\n\"", got)
	}

	oldSandbox := n.SandboxID
	if _, err := svc.restart(ctx, "c", n.ID); err != nil {
		t.Fatalf("restart non-persistent: %v", err)
	}
	if n.SandboxID == oldSandbox {
		t.Fatal("non-persistent restart should create a new sandbox")
	}
	if n.Generation != 2 {
		t.Fatalf("generation after non-persistent restart = %d, want 2", n.Generation)
	}
	if got := readGeneration(t, sub, n.SandboxID); got != "2\n" {
		t.Fatalf("guest file after non-persistent restart = %q, want \"2\\n\"", got)
	}
}

// TestGenerationCrashRecovery: after the orchestrator dies, a lease
// recovered from its checkpoint is on the next generation with the file
// rewritten; a lease lost in the crash is left untouched (there is no
// guest to write into and no process to inform).
func TestGenerationCrashRecovery(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := t.Context()

	ck, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "", "")
	if err != nil {
		t.Fatalf("grant checkpointed: %v", err)
	}
	if _, err := svc.checkpointLease(ctx, ck); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	bare, err := svc.grant(ctx, "c", "py-base", time.Minute, false, "", nil, "", "")
	if err != nil {
		t.Fatalf("grant bare: %v", err)
	}

	sub.Fake.Kill(ck.SandboxID)
	sub.Fake.Kill(bare.SandboxID)
	summary := svc.reconcileCrash(ctx)
	if summary.Recovered != 1 || summary.Lost != 1 {
		t.Fatalf("summary = %+v, want {Recovered:1, Lost:1}", summary)
	}

	if ck.State != "recovered" {
		t.Fatalf("state = %q, want recovered", ck.State)
	}
	if ck.Generation != 2 {
		t.Fatalf("generation after recovery = %d, want 2", ck.Generation)
	}
	if got := readGeneration(t, sub, ck.SandboxID); got != "2\n" {
		t.Fatalf("guest file after recovery = %q, want \"2\\n\"", got)
	}
	if bare.Generation != 1 {
		t.Fatalf("lost lease generation = %d, want 1", bare.Generation)
	}
}

// TestGenerationDrainUndrain: the admin drain/undrain pauses and resumes
// through the memory snapshot — no bump, file unchanged.
func TestGenerationDrainUndrain(t *testing.T) {
	ts, svc, _, sub := newAdminServer(t, "admin-tok")
	ctx := t.Context()

	l, err := svc.grant(ctx, "c", "py-base", time.Minute, false, "", nil, "", "")
	if err != nil {
		t.Fatalf("grant: %v", err)
	}

	resp, body := doReq(t, "POST", ts.URL+"/api/admin/drain", "admin-tok", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("drain = %d (%v), want 200", resp.StatusCode, body)
	}
	if l.Generation != 1 {
		t.Fatalf("generation after drain = %d, want 1", l.Generation)
	}

	resp, body = doReq(t, "POST", ts.URL+"/api/admin/undrain", "admin-tok", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("undrain = %d (%v), want 200", resp.StatusCode, body)
	}
	if !l.live() {
		t.Fatalf("lease not live after undrain: %+v", l)
	}
	if l.Generation != 1 {
		t.Fatalf("generation after undrain = %d, want 1", l.Generation)
	}
	if got := readGeneration(t, sub, l.SandboxID); got != "1\n" {
		t.Fatalf("guest file after undrain = %q, want \"1\\n\"", got)
	}
}

// TestGenerationWriteFailureIsBestEffort: a failing guest write is
// logged and otherwise ignored — the lease still comes up and restarts.
func TestGenerationWriteFailureIsBestEffort(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := t.Context()

	l, err := svc.grant(ctx, "c", "py-base", time.Minute, false, "", nil, "", "")
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	// Every future WriteFile fails (the file write on restart, for
	// instance): the restart must still succeed.
	sub.FailCall("WriteFile", 0, errors.New("disk gone"))
	if _, err := svc.restart(ctx, "c", l.ID); err != nil {
		t.Fatalf("restart with a failing generation write: %v", err)
	}
	if l.Generation != 2 {
		t.Fatalf("generation = %d, want 2", l.Generation)
	}
}

// TestGenerationCloneFork: clone and fork leases start on generation 1
// with the file in their guest.
func TestGenerationCloneFork(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := t.Context()

	src, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "", "")
	if err != nil {
		t.Fatalf("grant: %v", err)
	}

	cloned, _, err := svc.clone(ctx, "c", src.ID)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	if cloned.Generation != 1 {
		t.Fatalf("clone generation = %d, want 1", cloned.Generation)
	}
	if got := readGeneration(t, sub, cloned.SandboxID); got != "1\n" {
		t.Fatalf("clone guest file = %q, want \"1\\n\"", got)
	}

	forks, _, err := svc.fork(ctx, "c", src.ID, 2, false, time.Minute, "", "")
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	for _, f := range forks {
		if f.Generation != 1 {
			t.Fatalf("fork generation = %d, want 1", f.Generation)
		}
		if got := readGeneration(t, sub, f.SandboxID); got != "1\n" {
			t.Fatalf("fork guest file = %q, want \"1\\n\"", got)
		}
	}
}

// TestGenerationResumeRunning: resuming a lease that is already running
// restores its pause build again, so the memory rolls back and the
// generation bumps.
func TestGenerationResumeRunning(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := t.Context()

	p, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "", "")
	if err != nil {
		t.Fatalf("grant persistent: %v", err)
	}
	if _, err := svc.suspend(ctx, "c", p.ID); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if _, err := svc.resume(ctx, "c", p.ID); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if _, err := svc.resume(ctx, "c", p.ID); err != nil {
		t.Fatalf("resume of a running lease: %v", err)
	}
	if p.Generation != 2 {
		t.Fatalf("generation after resuming a running lease = %d, want 2", p.Generation)
	}
	if got := readGeneration(t, sub, p.SandboxID); got != "2\n" {
		t.Fatalf("guest file = %q, want \"2\\n\"", got)
	}
}
