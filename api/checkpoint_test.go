package api

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestPeriodicCheckpointSkipsIdle: the periodic pass checkpoints a
// live lease whose own interval is due and that was active since its
// last checkpoint, then skips it until the next activity. A lease with
// no interval (the host default 0 = never) and a suspended lease are
// never checkpointed — persistent alone no longer puts a lease on the
// pass (2.3, #122).
func TestPeriodicCheckpointSkipsIdle(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	l, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant persistent: %v", err)
	}
	plain, err := svc.grant(ctx, "c", "py-base", time.Minute, false, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant plain: %v", err)
	}

	// Neither lease has an interval of its own and the host default is
	// 0 = never: the pass does nothing despite activity.
	svc.checkpointIdleLeases(ctx)
	if got := calls(sub.Fake, "Checkpoint "+l.SandboxID); got != 0 {
		t.Fatalf("never-interval lease checkpointed %d times, want 0", got)
	}

	// Give the persistent lease its own interval: now it is due (active,
	// never checkpointed); the plain lease stays off the pass.
	svc.store.mu.Lock()
	l.CheckpointInterval = 60
	svc.saveLeaseLocked(l)
	svc.store.mu.Unlock()

	// First pass: the lease with an interval is due.
	svc.checkpointIdleLeases(ctx)
	if got := calls(sub.Fake, "Checkpoint "+l.SandboxID); got != 1 {
		t.Fatalf("due lease checkpointed %d times, want 1 (calls %v)", got, sub.Fake.CallLog())
	}
	if got := calls(sub.Fake, "Checkpoint "+plain.SandboxID); got != 0 {
		t.Fatalf("non-persistent lease checkpointed %d times, want 0", got)
	}

	// Second pass: no activity since the checkpoint → skipped.
	svc.checkpointIdleLeases(ctx)
	if got := calls(sub.Fake, "Checkpoint "+l.SandboxID); got != 1 {
		t.Fatalf("idle lease checkpointed again: %d total, want 1", got)
	}

	// Age the last checkpoint past the interval and touch the lease:
	// active since, and the interval has elapsed → due again.
	svc.store.mu.Lock()
	l.LastCheckpointAt = svc.now().Add(-2 * time.Minute)
	svc.saveLeaseLocked(l)
	svc.store.mu.Unlock()
	svc.touch(l.ID)
	svc.checkpointIdleLeases(ctx)
	if got := calls(sub.Fake, "Checkpoint "+l.SandboxID); got != 2 {
		t.Fatalf("re-armed lease checkpointed %d times, want 2", got)
	}

	// A suspended lease is not live and never checkpointed.
	if _, err := svc.suspend(ctx, "c", l.ID); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	svc.touch(l.ID)
	svc.checkpointIdleLeases(ctx)
	if got := calls(sub.Fake, "Checkpoint "+l.SandboxID); got != 2 {
		t.Fatalf("suspended lease checkpointed: %d total, want 2", got)
	}
}

// TestManualCheckpointRoute: the owner checkpoints a live lease and gets
// {"id","build_id","at"}; other owners get 404; a suspended lease gets
// 409.
func TestManualCheckpointRoute(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	l, err := svc.grant(ctx, "consumer-a", "py-base", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}

	resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes/"+l.ID+"/checkpoint", "token-a", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("checkpoint = %d (%v), want 200", resp.StatusCode, body)
	}
	if body["id"] != l.ID {
		t.Fatalf("checkpoint id = %v, want %s", body["id"], l.ID)
	}
	buildID, _ := body["build_id"].(string)
	if buildID == "" {
		t.Fatalf("checkpoint build_id empty: %v", body)
	}
	at, _ := body["at"].(string)
	if _, err := time.Parse(time.RFC3339, at); err != nil {
		t.Fatalf("checkpoint at = %q, want RFC 3339: %v", at, err)
	}
	if got := calls(sub.Fake, "Checkpoint "+l.SandboxID); got != 1 {
		t.Fatalf("checkpoint called %d times, want 1", got)
	}
	if l.LastCheckpointBuildID != buildID {
		t.Fatalf("lease LastCheckpointBuildID = %q, want %q", l.LastCheckpointBuildID, buildID)
	}

	// Another owner's lease is not found.
	resp, _ = doReq(t, "POST", ts.URL+"/api/sandboxes/"+l.ID+"/checkpoint", "token-b", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("checkpoint of another owner's lease = %d, want 404", resp.StatusCode)
	}

	// A suspended (not live) lease is a 409.
	if _, err := svc.suspend(ctx, "consumer-a", l.ID); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	resp, body = doReq(t, "POST", ts.URL+"/api/sandboxes/"+l.ID+"/checkpoint", "token-a", nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("checkpoint of a suspended lease = %d (%v), want 409", resp.StatusCode, body)
	}
}

// TestCheckpointHistogramObserved: every sub.Checkpoint call's duration
// lands in spoond_checkpoint_duration_seconds.
func TestCheckpointHistogramObserved(t *testing.T) {
	ts, _, db, _ := newTestServerWithService(t)
	seedImage(t, db, "py-base", 2048)
	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "ttl": 300, "persistent": true})
	id := create["id"].(string)

	if _, body := doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/checkpoint", "token-a", nil); body["build_id"] == nil {
		t.Fatalf("checkpoint failed: %v", body)
	}

	req, _ := http.NewRequest("GET", ts.URL+"/metrics", nil)
	req.Header.Set("Authorization", "Bearer token-a")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("metrics: %v", err)
	}
	out, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("read metrics: %v", err)
	}
	if !strings.Contains(string(out), "spoond_checkpoint_duration_seconds") {
		t.Fatalf("metrics output missing spoond_checkpoint_duration_seconds:\n%s", out)
	}
}
