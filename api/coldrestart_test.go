package api

import (
	"errors"
	"testing"
	"time"
)

// TestColdRestartPersistent: mode=cold on a persistent lease runs the
// fresh-guest path — the fake sees the sandbox deleted and a new one
// created — while the lease keeps its id, holder, name, policy and
// exposed ports. The generation bumps to 2 and the guest file is
// rewritten, the create-time secrets are re-written into the fresh
// sandbox, and the lease stays persistent (#120).
func TestColdRestartPersistent(t *testing.T) {
	svc, db, sub := newTestService(t)
	img := seedImage(t, db, "py-base", 2048)
	ctx := t.Context()

	l, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "internet", nil,
		"ci-flight-7", "https://ci.example.com/flight/7",
		map[string]string{"API_KEY": "s3cret"}, 8080, 9090)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := svc.setName("c", l.ID, "coldy"); err != nil {
		t.Fatalf("tag: %v", err)
	}
	oldSandbox := l.SandboxID

	// Give the lease a resume point the cold restart must clear.
	if _, err := svc.pauseLeaseBody(ctx, l, false, suspendPolicy{}); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if _, err := svc.resumeLease(ctx, l); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if l.ResumeBuildID == "" {
		t.Fatal("precondition: a resumed lease carries a resume_build_id")
	}

	n, err := svc.restart(ctx, "c", l.ID, "cold")
	if err != nil {
		t.Fatalf("cold restart: %v", err)
	}
	if n.ID != l.ID {
		t.Fatal("cold restart must keep the lease id")
	}
	if n.SandboxID == oldSandbox {
		t.Fatal("cold restart must create a new sandbox id")
	}
	if got := calls(sub.Fake, "Delete "+oldSandbox); got != 1 {
		t.Fatalf("expected the old sandbox deleted once, calls: %v", sub.Fake.CallLog())
	}
	if calls(sub.Fake, "Create") < 2 {
		t.Fatalf("expected a fresh create, calls: %v", sub.Fake.CallLog())
	}
	if !n.live() {
		t.Fatalf("cold-restarted lease not live: %+v", n)
	}
	if !n.Persistent {
		t.Fatal("a persistent lease cold-restarted stays persistent")
	}
	if n.Generation != 2 {
		t.Fatalf("generation after cold restart = %d, want 2", n.Generation)
	}
	if got := readGeneration(t, sub, n.SandboxID); got != "2\n" {
		t.Fatalf("guest file after cold restart = %q, want \"2\\n\"", got)
	}
	if n.ResumeBuildID != "" {
		t.Fatalf("resume_build_id = %q, want cleared (the next suspend sets it again)", n.ResumeBuildID)
	}
	// The fresh guest comes from the image's current build (here the
	// template build the lease originally started from; the pause build
	// it moved to during the round-trip is left behind).
	if n.BuildID != img.CurrentBuildID {
		t.Fatalf("build id = %q, want the image's current build %q", n.BuildID, img.CurrentBuildID)
	}
	// Identity the restart keeps: holder, name, policy, ports, owner.
	if n.Holder != "ci-flight-7" || n.HolderUrl != "https://ci.example.com/flight/7" {
		t.Fatalf("holder not kept: %+v", n)
	}
	if n.Name != "coldy" {
		t.Fatalf("name = %q, want coldy (kept)", n.Name)
	}
	if n.NetPolicy != "internet" {
		t.Fatalf("net policy = %q, want internet (kept)", n.NetPolicy)
	}
	if len(n.ExposePorts) != 2 || n.ExposePorts[0] != 8080 || n.ExposePorts[1] != 9090 {
		t.Fatalf("exposed ports = %v, want [8080 9090] (kept)", n.ExposePorts)
	}
	if n.Owner != "c" {
		t.Fatalf("owner = %q, want c (kept)", n.Owner)
	}
	// The create-time secret was re-written into the fresh sandbox.
	data, err := sub.Fake.ReadFile(ctx, n.SandboxID, secretPath("API_KEY"), 1024)
	if err != nil {
		t.Fatalf("read secret from fresh sandbox: %v", err)
	}
	if string(data) != "s3cret" {
		t.Fatalf("secret in fresh sandbox = %q, want s3cret", data)
	}
}

// TestColdRestartSuspended: mode=cold on a suspended lease does not
// resume its snapshot — it deletes the paused sandbox (already gone from
// the fake) and creates a fresh guest; the lease comes back running.
func TestColdRestartSuspended(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := t.Context()

	l, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := svc.suspend(ctx, "c", l.ID); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if !l.Suspended || l.ResumeBuildID == "" {
		t.Fatalf("precondition: lease not suspended with a resume point: %+v", l)
	}
	oldSandbox := l.SandboxID

	if _, err := svc.restart(ctx, "c", l.ID, "cold"); err != nil {
		t.Fatalf("cold restart: %v", err)
	}
	if !l.live() || l.Suspended {
		t.Fatalf("suspended lease cold-restarted must come back running: %+v", l)
	}
	if l.SandboxID == oldSandbox {
		t.Fatal("cold restart must create a new sandbox id")
	}
	if got := calls(sub.Fake, "Delete "+oldSandbox); got != 1 {
		t.Fatalf("expected the old sandbox id deleted once, calls: %v", sub.Fake.CallLog())
	}
	if l.Generation != 2 {
		t.Fatalf("generation after cold restart = %d, want 2", l.Generation)
	}
	if got := readGeneration(t, sub, l.SandboxID); got != "2\n" {
		t.Fatalf("guest file after cold restart = %q, want \"2\\n\"", got)
	}
	if l.ResumeBuildID != "" {
		t.Fatalf("resume_build_id = %q, want cleared", l.ResumeBuildID)
	}
	// A following suspend sets a fresh resume point as usual.
	if _, err := svc.suspend(ctx, "c", l.ID); err != nil {
		t.Fatalf("suspend after cold restart: %v", err)
	}
	if l.ResumeBuildID == "" {
		t.Fatal("the suspend after a cold restart must set a new resume_build_id")
	}
}

// TestColdRestartSuspendsThenColdAgain: after a cold restart a further
// warm restart is the plain persistent round-trip (generation stays).
func TestColdRestartThenWarm(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := t.Context()

	l, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := svc.restart(ctx, "c", l.ID, "cold"); err != nil {
		t.Fatalf("cold restart: %v", err)
	}
	if _, err := svc.restart(ctx, "c", l.ID, "warm"); err != nil {
		t.Fatalf("warm restart: %v", err)
	}
	if l.Generation != 2 {
		t.Fatalf("generation after a warm restart post-cold = %d, want 2 (warm continues the memory)", l.Generation)
	}
	if got := readGeneration(t, sub, l.SandboxID); got != "2\n" {
		t.Fatalf("guest file = %q, want \"2\\n\"", got)
	}
}

// TestRestartBadMode: any mode other than warm, cold or empty is a 400
// at the HTTP layer (and an error from the service).
func TestRestartBadMode(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := t.Context()

	l, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	for _, mode := range []string{"hot", "reboot", "COLD"} {
		if _, err := svc.restart(ctx, "c", l.ID, mode); err != errBadRestartMode {
			t.Fatalf("restart mode %q: err %v, want errBadRestartMode", mode, err)
		}
	}
}

// TestColdRestartCreateFailureKeepsLease: when the fresh guest cannot be
// created, the cold restart fails and the lease keeps its old sandbox
// (the old one is only deleted after the new one exists).
func TestColdRestartCreateFailureKeepsLease(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := t.Context()
	l, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	old := l.SandboxID
	sub.FailCall("Create", 0, errors.New("no capacity"))
	if _, err := svc.restart(ctx, "c", l.ID, "cold"); err == nil {
		t.Fatal("cold restart with a failing create succeeded")
	}
	if l.SandboxID != old {
		t.Fatalf("sandbox changed to %q after a failed cold restart, want %q", l.SandboxID, old)
	}
	for _, c := range sub.CallLog() {
		if c == "Delete "+old {
			t.Fatal("the old sandbox was deleted although no new one was created")
		}
	}
}
