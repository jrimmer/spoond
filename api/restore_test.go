package api

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v2/substrate"
)

// TestRestoreToKeptBuild: restore replaces the lease's sandbox with one
// from a kept checkpoint while keeping the lease's id, holder, name,
// policy and exposed ports; the generation bumps to 2, the guest file
// is rewritten, the create-time secrets re-staged, and a "restored"
// event names the build.
func TestRestoreToKeptBuild(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := t.Context()

	l, err := svc.grant(ctx, "c", "py-base", time.Hour, true, "internet", nil,
		"ci-flight-7", "https://ci.example.com/flight/7",
		map[string]string{"API_KEY": "s3cret"}, 8080, 9090)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := svc.setName("c", l.ID, "restory"); err != nil {
		t.Fatalf("tag: %v", err)
	}
	oldSandbox := l.SandboxID

	b, err := svc.checkpointLease(ctx, l)
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if err := svc.keepBuild(ctx, l.ID, b.BuildID); err != nil {
		t.Fatalf("keep: %v", err)
	}

	all := svc.Subscribe(EventFilter{})

	if err := svc.restoreBusy(ctx, l, b); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if l.SandboxID == oldSandbox {
		t.Fatal("restore must create a new sandbox id")
	}
	if got := calls(sub.Fake, "Delete "+oldSandbox); got != 1 {
		t.Fatalf("old sandbox deleted %d times, want 1 (calls: %v)", got, sub.Fake.CallLog())
	}
	if !l.live() || l.Suspended {
		t.Fatalf("restored lease must be live: %+v", l)
	}
	if l.ID == "" || l.Holder != "ci-flight-7" || l.Name != "restory" ||
		l.NetPolicy != "internet" || len(l.ExposePorts) != 2 || l.ExposePorts[0] != 8080 {
		t.Fatalf("identity not kept: %+v", l)
	}
	if l.BuildID != b.BuildID {
		t.Fatalf("lease build id = %q, want the restored build %q", l.BuildID, b.BuildID)
	}
	if l.Generation != 2 {
		t.Fatalf("generation after restore = %d, want 2", l.Generation)
	}
	if got := readGeneration(t, sub, l.SandboxID); got != "2\n" {
		t.Fatalf("guest file after restore = %q, want \"2\\n\"", got)
	}
	data, err := sub.Fake.ReadFile(ctx, l.SandboxID, secretPath("API_KEY"), 1024)
	if err != nil {
		t.Fatalf("read secret from restored sandbox: %v", err)
	}
	if string(data) != "s3cret" {
		t.Fatalf("secret in restored sandbox = %q, want s3cret", data)
	}

	all.Close()
	var found bool
	for _, ev := range eventsFor(collectEvents(all.C), l.ID) {
		if ev.Type == LeaseRestored {
			found = true
			if !strings.Contains(ev.Detail, b.BuildID) {
				t.Fatalf("restored detail = %q, want the build id", ev.Detail)
			}
		}
	}
	if !found {
		t.Fatal("no restored event emitted")
	}
}

// TestRestoreSuspendedLease: a suspended lease restores in place and
// comes back running, its resume point cleared.
func TestRestoreSuspendedLease(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := t.Context()

	l, err := svc.grant(ctx, "c", "py-base", time.Hour, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := svc.checkpointLease(ctx, l); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if _, err := svc.suspend(ctx, "c", l.ID); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if !l.Suspended || l.ResumeBuildID == "" {
		t.Fatalf("precondition: not suspended with a resume point: %+v", l)
	}
	// The lease's newest checkpoint is restorable even while suspended.
	b, err := db.GetBuild(ctx, l.LastCheckpointBuildID)
	if err != nil {
		t.Fatalf("get build: %v", err)
	}
	if err := svc.restoreBusy(ctx, l, b); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if !l.live() || l.Suspended {
		t.Fatalf("restored-from-suspended lease must be running: %+v", l)
	}
	if l.ResumeBuildID != "" {
		t.Fatalf("resume_build_id = %q, want cleared", l.ResumeBuildID)
	}
	if l.Generation != 2 {
		t.Fatalf("generation = %d, want 2", l.Generation)
	}
}

// TestRestoreForeignBuild404: another lease's checkpoint and another
// owner's build are both 404 — no existence leak.
func TestRestoreForeignBuild404(t *testing.T) {
	ts, svc, db, _ := newTestServerWithService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	l, err := svc.grant(ctx, "consumer-a", "py-base", time.Hour, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := svc.checkpointLease(ctx, l); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	other, err := svc.grant(ctx, "consumer-a", "py-base", time.Hour, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant other: %v", err)
	}
	foreign, err := svc.checkpointLease(ctx, other)
	if err != nil {
		t.Fatalf("checkpoint other: %v", err)
	}

	restoreAs := func(token, leaseID, buildID string) int {
		t.Helper()
		resp, _ := doReq(t, "POST", ts.URL+"/api/leases/"+leaseID+"/restore", token,
			map[string]any{"build_id": buildID})
		return resp.StatusCode
	}

	// Another lease's checkpoint (same owner): not one of this lease's
	// own checkpoints or kept builds → 404.
	if got := restoreAs("token-a", l.ID, foreign.BuildID); got != http.StatusNotFound {
		t.Fatalf("restore to another lease's checkpoint = %d, want 404", got)
	}

	// Another owner's kept build → 404 even for its own lease.
	if err := svc.keepBuild(ctx, other.ID, foreign.BuildID); err != nil {
		t.Fatalf("keep: %v", err)
	}
	if got := restoreAs("token-b", l.ID, foreign.BuildID); got != http.StatusNotFound {
		t.Fatalf("restore by another owner = %d, want 404", got)
	}

	// Unknown build id → 404.
	if got := restoreAs("token-a", l.ID, "00000000-0000-0000-0000-000000000000"); got != http.StatusNotFound {
		t.Fatalf("restore to unknown build = %d, want 404", got)
	}

	// A pause build of the lease is not a checkpoint nor kept → 404.
	pauseID, err := svc.pauseLease(ctx, l, false)
	if err != nil {
		t.Fatalf("pause: %v", err)
	}
	if got := restoreAs("token-a", l.ID, pauseID); got != http.StatusNotFound {
		t.Fatalf("restore to a pause build = %d, want 404", got)
	}
}

// TestRestoreBusy409: a restore on a busy lease is 409, and another
// operation during a restore in flight is 409 too (the restore holds
// the busy window while the substrate create is slow).
func TestRestoreBusy409(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := t.Context()

	l, err := svc.grant(ctx, "c", "py-base", time.Hour, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	b, err := svc.checkpointLease(ctx, l)
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if err := svc.keepBuild(ctx, l.ID, b.BuildID); err != nil {
		t.Fatalf("keep: %v", err)
	}

	// Mark the lease busy (as an in-flight operation would).
	svc.store.mu.Lock()
	l.busy = true
	svc.store.mu.Unlock()
	err = svc.restoreBusy(ctx, l, b)
	if err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("restore on a busy lease = %v, want errLeaseBusy", err)
	}
	svc.store.mu.Lock()
	l.busy = false
	svc.store.mu.Unlock()

	// During a restore in flight (a slow NodeInfo inside admit), a
	// checkpoint is 409.
	var once sync.Once
	release := make(chan struct{})
	started := make(chan struct{})
	sub.Fake.SetNodeInfoFunc(func(ctx context.Context) (substrate.NodeInfo, error) {
		once.Do(func() { close(started) })
		<-release
		// Clear the override first: NodeInfo routes back into this fn,
		// and recursing here would never see the release.
		sub.Fake.SetNodeInfoFunc(nil)
		return sub.Fake.NodeInfo(ctx)
	})
	done := make(chan error, 1)
	go func() { done <- svc.restoreBusy(ctx, l, b) }()
	<-started
	if _, err := svc.checkpointLeaseBusy(ctx, l); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("checkpoint during restore = %v, want errLeaseBusy", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("restore: %v", err)
	}
}

// TestRestoreRouteAdminAndBody: the route is owner or admin, others 404;
// the response carries the new generation; a bad body is 400.
func TestRestoreRouteAdminAndBody(t *testing.T) {
	ts, svc, db, _ := newTestServerWithService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	l, err := svc.grant(ctx, "consumer-a", "py-base", time.Hour, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	b, err := svc.checkpointLease(ctx, l)
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if err := svc.keepBuild(ctx, l.ID, b.BuildID); err != nil {
		t.Fatalf("keep: %v", err)
	}

	// Owner restores: 200 with the generation in the body.
	resp, body := doReq(t, "POST", ts.URL+"/api/leases/"+l.ID+"/restore", "token-a",
		map[string]any{"build_id": b.BuildID})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("restore = %d (%v), want 200", resp.StatusCode, body)
	}
	if body["generation"].(float64) != 2 {
		t.Fatalf("restore generation = %v, want 2", body["generation"])
	}
	if body["build_id"] != b.BuildID {
		t.Fatalf("restore build_id = %v, want %s", body["build_id"], b.BuildID)
	}

	// Admin can restore someone else's lease; others get 404.
	b2, err := svc.checkpointLease(ctx, l)
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if err := svc.keepBuild(ctx, l.ID, b2.BuildID); err != nil {
		t.Fatalf("keep: %v", err)
	}
	resp, _ = doReq(t, "POST", ts.URL+"/api/leases/"+l.ID+"/restore", "token-b",
		map[string]any{"build_id": b2.BuildID})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("restore by a stranger = %d, want 404", resp.StatusCode)
	}

	// A missing/invalid body is 400 — but only after the lease itself
	// has answered 404 for callers who cannot see it (no existence
	// leak; 1bcbf56's ordering).
	req, _ := http.NewRequest("POST", ts.URL+"/api/leases/"+l.ID+"/restore",
		strings.NewReader("{not json"))
	req.Header.Set("Authorization", "Bearer token-a")
	hresp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	hresp.Body.Close()
	if hresp.StatusCode != http.StatusBadRequest {
		t.Fatalf("restore bad body = %d, want 400", hresp.StatusCode)
	}
	// A stranger gets the lease 404 even with a bad body.
	req, _ = http.NewRequest("POST", ts.URL+"/api/leases/"+l.ID+"/restore",
		strings.NewReader("{not json"))
	req.Header.Set("Authorization", "Bearer token-b")
	hresp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	hresp.Body.Close()
	if hresp.StatusCode != http.StatusNotFound {
		t.Fatalf("restore bad body by a stranger = %d, want 404", hresp.StatusCode)
	}
}
