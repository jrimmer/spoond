package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v2/identity"
	"github.com/jrimmer/spoond/v2/store"
	"github.com/jrimmer/spoond/v2/substrate"
	"github.com/jrimmer/spoond/v2/substrate/e2b"
)

// createLiveLease creates a running lease over the API as token-a and
// returns its id.
func createLiveLease(t *testing.T, ts, token string, body map[string]any) string {
	t.Helper()
	if body == nil {
		body = map[string]any{"image": "py-base", "ttl": 300}
	}
	resp, out := doReq(t, "POST", ts+"/api/sandboxes", token, body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create lease: status %d: %v", resp.StatusCode, out)
	}
	return out["id"].(string)
}

// saveSnapshot saves a lease as a named snapshot over the API.
func saveSnapshot(t *testing.T, ts, token, leaseID string, body map[string]any) (*http.Response, map[string]any) {
	t.Helper()
	return doReq(t, "POST", ts+"/api/leases/"+leaseID+"/snapshots", token, body)
}

// TestNamedSnapshotSaveAndList: a save checkpoints and stores a version;
// the list shows it with in_use 0 and stale false.
func TestNamedSnapshotSaveAndList(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	svc.cfg.TemplateStoragePath = t.TempDir()
	seedImage(t, db, "py-base", 2048)
	id := createLiveLease(t, ts.URL, "token-a", nil)

	resp, body := saveSnapshot(t, ts.URL, "token-a", id, map[string]any{"name": "spoond/warm"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("save: status %d: %v", resp.StatusCode, body)
	}
	if body["name"] != "spoond/warm" || body["version"].(float64) != 1 {
		t.Fatalf("save body = %v", body)
	}
	if body["build_id"] == "" {
		t.Fatalf("save body missing build_id: %v", body)
	}
	if calls(sub.Fake, "Checkpoint") != 1 {
		t.Fatalf("checkpoint calls = %d, want 1", calls(sub.Fake, "Checkpoint"))
	}

	resp, list := doReq(t, "GET", ts.URL+"/api/named-snapshots", "token-a", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: status %d: %v", resp.StatusCode, list)
	}
	snaps := list["snapshots"].([]any)
	if len(snaps) != 1 {
		t.Fatalf("list = %v, want one name", list)
	}
	names := snaps[0].(map[string]any)
	if names["name"] != "spoond/warm" || names["latest"].(float64) != 1 {
		t.Fatalf("list name = %v", names)
	}
	vs := names["versions"].([]any)
	if len(vs) != 1 {
		t.Fatalf("versions = %v", vs)
	}
	v := vs[0].(map[string]any)
	if v["in_use"].(float64) != 0 || v["stale"].(bool) {
		t.Fatalf("version = %v, want in_use 0 stale false", v)
	}
}

// TestNamedSnapshotIdempotentReplay: a replay with the same key answers
// 200 with the same version and takes no checkpoint. A replay from a
// second lease (A1) answers the same version too.
func TestNamedSnapshotIdempotentReplay(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	svc.cfg.TemplateStoragePath = t.TempDir()
	seedImage(t, db, "py-base", 2048)
	a := createLiveLease(t, ts.URL, "token-a", nil)
	b := createLiveLease(t, ts.URL, "token-a", nil)

	resp, first := saveSnapshot(t, ts.URL, "token-a", a, map[string]any{"name": "warm", "idempotency_key": "k1"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("first save: status %d: %v", resp.StatusCode, first)
	}
	checkpoints := calls(sub.Fake, "Checkpoint")

	// Replay from the same lease.
	resp, replay := saveSnapshot(t, ts.URL, "token-a", a, map[string]any{"name": "warm", "idempotency_key": "k1"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("replay: status %d: %v", resp.StatusCode, replay)
	}
	if replay["version"].(float64) != first["version"].(float64) || replay["build_id"] != first["build_id"] {
		t.Fatalf("replay = %v, want %v", replay, first)
	}
	// Replay from a second lease: the key is not lease-scoped (A1).
	resp, replayB := saveSnapshot(t, ts.URL, "token-a", b, map[string]any{"name": "warm", "idempotency_key": "k1"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("replay from lease b: status %d: %v", resp.StatusCode, replayB)
	}
	if replayB["build_id"] != first["build_id"] {
		t.Fatalf("replay from b = %v, want %v", replayB, first)
	}
	if got := calls(sub.Fake, "Checkpoint"); got != checkpoints {
		t.Fatalf("replays took %d checkpoint(s), want 0", got-checkpoints)
	}
}

// TestNamedSnapshotInFlightConflict: while a save with a key is in
// flight, a concurrent save with the same key answers 409
// save_in_progress with Retry-After.
func TestNamedSnapshotInFlightConflict(t *testing.T) {
	ts, svc, db, _ := newTestServerWithService(t)
	svc.cfg.TemplateStoragePath = t.TempDir()
	seedImage(t, db, "py-base", 2048)
	id := createLiveLease(t, ts.URL, "token-a", nil)

	// Hold the key in flight by claiming it directly, then ask for a
	// save with it.
	if !svc.saves.begin("consumer-a", "warm", "k1") {
		t.Fatal("begin should claim a fresh key")
	}
	resp, body := saveSnapshot(t, ts.URL, "token-a", id, map[string]any{"name": "warm", "idempotency_key": "k1"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("in-flight save: status %d: %v", resp.StatusCode, body)
	}
	if body["code"] != "save_in_progress" {
		t.Fatalf("in-flight body = %v, want code save_in_progress", body)
	}
	if resp.Header.Get("Retry-After") != "5" {
		t.Fatalf("Retry-After = %q, want 5", resp.Header.Get("Retry-After"))
	}
}

// TestNamedSnapshotFailedSaveDoesNotPoisonKey: a failed save leaves no
// version and a replay with the same key runs a fresh save.
func TestNamedSnapshotFailedSaveDoesNotPoisonKey(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	svc.cfg.TemplateStoragePath = t.TempDir()
	seedImage(t, db, "py-base", 2048)
	id := createLiveLease(t, ts.URL, "token-a", nil)

	// First attempt: the checkpoint fails.
	sub.FailCall("Checkpoint", 1, errors.New("orchestrator exploded"))
	resp, body := saveSnapshot(t, ts.URL, "token-a", id, map[string]any{"name": "warm", "idempotency_key": "k1"})
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("failed save: status %d: %v", resp.StatusCode, body)
	}
	if _, err := db.GetNamedSnapshotLatest(context.Background(), "consumer-a", "warm"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("failed save left a version row: %v", err)
	}
	// The lookup reads failed.
	resp, lookup := doReq(t, "GET", ts.URL+"/api/named-snapshots/warm?idempotency_key=k1", "token-a", nil)
	if resp.StatusCode != http.StatusOK || lookup["state"] != "failed" {
		t.Fatalf("lookup after failure = %d %v, want state failed", resp.StatusCode, lookup)
	}
	// A replay runs a fresh save.
	resp, body = saveSnapshot(t, ts.URL, "token-a", id, map[string]any{"name": "warm", "idempotency_key": "k1"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("replay after failure: status %d: %v", resp.StatusCode, body)
	}
	if body["version"].(float64) != 1 {
		t.Fatalf("replay version = %v, want 1", body["version"])
	}
}

// TestNamedSnapshotInterruptedSave: a save interrupted between the
// checkpoint and the row insert leaves no version; a replay with the
// same key creates version 1 (A7). A checkpoint build still `building`
// with no named row is marked failed at startup.
func TestNamedSnapshotInterruptedSave(t *testing.T) {
	ts, svc, db, _ := newTestServerWithService(t)
	svc.cfg.TemplateStoragePath = t.TempDir()
	seedImage(t, db, "py-base", 2048)
	id := createLiveLease(t, ts.URL, "token-a", nil)

	// The fake hook fires once, after the checkpoint and before the row
	// insert: the build is written but never named, exactly as when the
	// backend stops mid-save. The save answers 500 and stores nothing.
	var interruptedBuild string
	svc.saveInterrupt = func(ctx context.Context, l *Lease, buildID string) error {
		interruptedBuild = buildID
		svc.saveInterrupt = nil
		return errors.New("backend stopped")
	}
	resp, body := saveSnapshot(t, ts.URL, "token-a", id, map[string]any{"name": "warm", "idempotency_key": "k1"})
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("interrupted save: status %d: %v", resp.StatusCode, body)
	}
	if _, err := db.GetNamedSnapshotLatest(context.Background(), "consumer-a", "warm"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("interrupted save left a version row: %v", err)
	}
	if interruptedBuild == "" {
		t.Fatal("saveInterrupt did not run; the save never reached the row insert")
	}
	// A backend restart loses the in-memory failure: the key reads
	// absent, and a replay with the same key runs a fresh save and
	// creates version 1 (A7).
	svc.saves = namedSaveInFlight{saves: map[string]*namedSaveState{}}
	resp, lookup := doReq(t, "GET", ts.URL+"/api/named-snapshots/warm?idempotency_key=k1", "token-a", nil)
	if resp.StatusCode != http.StatusOK || lookup["state"] != "absent" {
		t.Fatalf("lookup = %d %v, want absent after a restart", resp.StatusCode, lookup)
	}
	resp, replay := saveSnapshot(t, ts.URL, "token-a", id, map[string]any{"name": "warm", "idempotency_key": "k1"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("replay: status %d: %v", resp.StatusCode, replay)
	}
	if replay["version"].(float64) != 1 {
		t.Fatalf("replay version = %v, want 1", replay["version"])
	}

	// Startup marks a `building` checkpoint with no named row failed.
	stranded := e2b.NewUUID()
	now := time.Now()
	if err := db.InsertBuild(context.Background(), store.BuildRow{
		BuildID: stranded, Kind: "checkpoint", TemplateID: e2b.NewTemplateID(),
		Image: "py-base", State: "building", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("insert stranded build: %v", err)
	}
	if err := svc.LoadState(t.Context()); err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	b, err := db.GetBuild(context.Background(), stranded)
	if err != nil {
		t.Fatalf("get stranded: %v", err)
	}
	if b.State != "failed" {
		t.Fatalf("stranded build state = %q, want failed", b.State)
	}
}

// TestNamedSnapshotVersionsAndLatest: two saves make versions 1 and 2,
// latest moves, and resolving @1 still works.
func TestNamedSnapshotVersionsAndLatest(t *testing.T) {
	ts, svc, db, _ := newTestServerWithService(t)
	svc.cfg.TemplateStoragePath = t.TempDir()
	seedImage(t, db, "py-base", 2048)
	id := createLiveLease(t, ts.URL, "token-a", nil)

	v1 := saveSnapshotOK(t, ts.URL, "token-a", id, map[string]any{"name": "warm"})
	v2 := saveSnapshotOK(t, ts.URL, "token-a", id, map[string]any{"name": "warm"})
	if v1 != 1 || v2 != 2 {
		t.Fatalf("versions = %d, %d, want 1, 2", v1, v2)
	}
	resp, latest := doReq(t, "GET", ts.URL+"/api/named-snapshots/warm", "token-a", nil)
	if resp.StatusCode != http.StatusOK || latest["version"].(float64) != 2 {
		t.Fatalf("latest = %d %v, want version 2", resp.StatusCode, latest)
	}
	resp, pinned := doReq(t, "GET", ts.URL+"/api/named-snapshots/warm@1", "token-a", nil)
	if resp.StatusCode != http.StatusOK || pinned["version"].(float64) != 1 {
		t.Fatalf("@1 = %d %v, want version 1", resp.StatusCode, pinned)
	}
}

// saveSnapshotOK saves and asserts 201, returning the version.
func saveSnapshotOK(t *testing.T, ts, token, leaseID string, body map[string]any) int64 {
	t.Helper()
	resp, out := saveSnapshot(t, ts, token, leaseID, body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("save: status %d: %v", resp.StatusCode, out)
	}
	return int64(out["version"].(float64))
}

// TestNamedSnapshotNameValidation: invalid names are 400, valid
// project-scoped names are accepted.
func TestNamedSnapshotNameValidation(t *testing.T) {
	ts, svc, db, _ := newTestServerWithService(t)
	svc.cfg.TemplateStoragePath = t.TempDir()
	seedImage(t, db, "py-base", 2048)
	id := createLiveLease(t, ts.URL, "token-a", nil)

	for _, bad := range []string{"", "Warm", "a/b/c", "-warm", "spoond/", "/warm", strings.Repeat("a", 64)} {
		resp, body := saveSnapshot(t, ts.URL, "token-a", id, map[string]any{"name": bad})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("name %q: status %d (%v), want 400", bad, resp.StatusCode, body)
		}
	}
	for _, good := range []string{"warm", "spoond/warm", "a-b", "a.b_c", "x1"} {
		resp, body := saveSnapshot(t, ts.URL, "token-a", id, map[string]any{"name": good})
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("name %q: status %d (%v), want 201", good, resp.StatusCode, body)
		}
	}
}

// TestNamedSnapshotRetention: keep=1 drops older versions after a save,
// but never a version a live lease started from.
func TestNamedSnapshotRetention(t *testing.T) {
	ts, svc, db, _ := newTestServerWithService(t)
	svc.cfg.TemplateStoragePath = t.TempDir()
	seedImage(t, db, "py-base", 2048)
	id := createLiveLease(t, ts.URL, "token-a", nil)

	v1 := saveSnapshotOK(t, ts.URL, "token-a", id, map[string]any{"name": "warm", "keep": 1})
	v2 := saveSnapshotOK(t, ts.URL, "token-a", id, map[string]any{"name": "warm", "keep": 1})
	if v1 != 1 || v2 != 2 {
		t.Fatalf("versions = %d, %d", v1, v2)
	}
	// v1 is beyond keep=1 and no lease uses it: pruned.
	if _, err := db.GetNamedSnapshot(context.Background(), "consumer-a", "warm", 1); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("v1 still present after keep=1: %v", err)
	}

	// v2 is in use by a live lease (set snapshot_build_id directly): a
	// save that would drop it must not.
	row, err := db.GetNamedSnapshotLatest(context.Background(), "consumer-a", "warm")
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	leaseRow, err := db.GetLease(context.Background(), id)
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	leaseRow.SnapshotBuildID = row.BuildID
	if err := db.UpsertLease(context.Background(), leaseRow); err != nil {
		t.Fatalf("set snapshot_build_id: %v", err)
	}
	svc.store.mu.Lock()
	svc.store.leases[id].SnapshotBuildID = row.BuildID
	svc.store.mu.Unlock()
	saveSnapshotOK(t, ts.URL, "token-a", id, map[string]any{"name": "warm", "keep": 1})
	if _, err := db.GetNamedSnapshot(context.Background(), "consumer-a", "warm", 2); err != nil {
		t.Fatalf("in-use v2 was pruned: %v", err)
	}
}

// TestNamedSnapshotKeepPut: PUT changes retention and applies it at
// once.
func TestNamedSnapshotKeepPut(t *testing.T) {
	ts, svc, db, _ := newTestServerWithService(t)
	svc.cfg.TemplateStoragePath = t.TempDir()
	seedImage(t, db, "py-base", 2048)
	id := createLiveLease(t, ts.URL, "token-a", nil)
	for i := 0; i < 3; i++ {
		saveSnapshotOK(t, ts.URL, "token-a", id, map[string]any{"name": "warm"})
	}
	resp, body := doReq(t, "PUT", ts.URL+"/api/named-snapshots/warm", "token-a", map[string]any{"keep": 1})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put keep: status %d: %v", resp.StatusCode, body)
	}
	rows, err := db.ListNamedSnapshots(context.Background(), "consumer-a", "warm")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 || rows[0].Version != 3 {
		t.Fatalf("rows after keep=1 = %+v, want only v3", rows)
	}
}

// TestNamedSnapshotKeepFirstSaveOnly: the first save's keep is stored
// per name; later saves ignore their own keep field (A6).
func TestNamedSnapshotKeepFirstSaveOnly(t *testing.T) {
	ts, svc, db, _ := newTestServerWithService(t)
	svc.cfg.TemplateStoragePath = t.TempDir()
	seedImage(t, db, "py-base", 2048)
	id := createLiveLease(t, ts.URL, "token-a", nil)

	saveSnapshotOK(t, ts.URL, "token-a", id, map[string]any{"name": "warm", "keep": 2})
	// A later save asking keep=1 must not lower the stored setting.
	saveSnapshotOK(t, ts.URL, "token-a", id, map[string]any{"name": "warm", "keep": 1})
	rows, err := db.ListNamedSnapshots(context.Background(), "consumer-a", "warm")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("versions after keep=1 second save = %d, want 2 (first save's keep=2 holds)", len(rows))
	}
	// A third save prunes v1 (keep=2 keeps v2, v3).
	saveSnapshotOK(t, ts.URL, "token-a", id, map[string]any{"name": "warm", "keep": 1})
	rows, _ = db.ListNamedSnapshots(context.Background(), "consumer-a", "warm")
	if len(rows) != 2 || rows[0].Version != 3 || rows[1].Version != 2 {
		t.Fatalf("versions = %+v, want v3,v2", rows)
	}
}

// TestNamedSnapshotNameLimit: MAX_NAMED_SNAPSHOTS caps distinct names.
func TestNamedSnapshotNameLimit(t *testing.T) {
	ts, svc, db, _ := newTestServerWithService(t)
	svc.cfg.TemplateStoragePath = t.TempDir()
	svc.cfg.MaxNamedSnapshots = 1
	seedImage(t, db, "py-base", 2048)
	id := createLiveLease(t, ts.URL, "token-a", nil)

	saveSnapshotOK(t, ts.URL, "token-a", id, map[string]any{"name": "one"})
	resp, body := saveSnapshot(t, ts.URL, "token-a", id, map[string]any{"name": "two"})
	if resp.StatusCode != http.StatusConflict || body["code"] != "snapshot_limit" {
		t.Fatalf("second name: status %d (%v), want 409 snapshot_limit", resp.StatusCode, body)
	}
	// The existing name is still savable.
	if v := saveSnapshotOK(t, ts.URL, "token-a", id, map[string]any{"name": "one"}); v != 2 {
		t.Fatalf("existing name version = %d, want 2", v)
	}
}

// TestNamedSnapshotSecretsScrub: create-time secrets are removed before
// the checkpoint and re-staged after.
func TestNamedSnapshotSecretsScrub(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	svc.cfg.TemplateStoragePath = t.TempDir()
	seedImage(t, db, "py-base", 2048)
	id := createLiveLease(t, ts.URL, "token-a", map[string]any{
		"image": "py-base", "ttl": 300, "secrets": map[string]string{"TOKEN": "s3cr3t"},
	})
	// The create staged it.
	if _, err := sub.Fake.ReadFile(t.Context(), svc.store.leases[id].SandboxID, "/run/secrets/TOKEN", 1024); err != nil {
		t.Fatalf("create-time secret missing before save: %v", err)
	}
	sandbox := svc.store.leases[id].SandboxID

	var checkpointErr error
	sub.checkpointFn = func(ctx context.Context, sandboxID string) (string, substrate.BuildRefs, error) {
		_, checkpointErr = sub.Fake.ReadFile(ctx, sandboxID, "/run/secrets/TOKEN", 1024)
		return e2b.NewUUID(), substrate.BuildRefs{}, nil
	}
	saveSnapshotOK(t, ts.URL, "token-a", id, map[string]any{"name": "warm"})
	if checkpointErr == nil {
		t.Fatal("secret was still present at checkpoint time; want it scrubbed")
	}
	// Re-staged on the source after the checkpoint.
	got, err := sub.Fake.ReadFile(t.Context(), sandbox, "/run/secrets/TOKEN", 1024)
	if err != nil || string(got) != "s3cr3t" {
		t.Fatalf("secret after save = %q (%v), want re-staged", got, err)
	}
}

// TestNamedSnapshotExecSecretsConflict: a save while an exec-time secret
// is staged answers 409 secrets_in_use.
func TestNamedSnapshotExecSecretsConflict(t *testing.T) {
	ts, svc, db, _ := newTestServerWithService(t)
	svc.cfg.TemplateStoragePath = t.TempDir()
	seedImage(t, db, "py-base", 2048)
	id := createLiveLease(t, ts.URL, "token-a", nil)

	svc.markExecSecretsStaged(id, []string{"EXECTOK"})
	defer svc.unmarkExecSecretsStaged(id, []string{"EXECTOK"})
	resp, body := saveSnapshot(t, ts.URL, "token-a", id, map[string]any{"name": "warm"})
	if resp.StatusCode != http.StatusConflict || body["code"] != "secrets_in_use" {
		t.Fatalf("save with staged exec secrets: status %d (%v), want 409 secrets_in_use", resp.StatusCode, body)
	}
}

// TestNamedSnapshotDelete: in-use 409, force drops the row, plain 204 and
// the second delete 404.
func TestNamedSnapshotDelete(t *testing.T) {
	ts, svc, db, _ := newTestServerWithService(t)
	svc.cfg.TemplateStoragePath = t.TempDir()
	seedImage(t, db, "py-base", 2048)
	id := createLiveLease(t, ts.URL, "token-a", nil)
	saveSnapshotOK(t, ts.URL, "token-a", id, map[string]any{"name": "warm"})

	// Not in use: 204, then 404.
	resp, _ := doReq(t, "DELETE", ts.URL+"/api/named-snapshots/warm", "token-a", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: status %d, want 204", resp.StatusCode)
	}
	resp, _ = doReq(t, "DELETE", ts.URL+"/api/named-snapshots/warm", "token-a", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("re-delete: status %d, want 404", resp.StatusCode)
	}

	// In use: 409 snapshot_in_use; force drops it.
	saveSnapshotOK(t, ts.URL, "token-a", id, map[string]any{"name": "cold"})
	row, _ := db.GetNamedSnapshotLatest(context.Background(), "consumer-a", "cold")
	leaseRow, _ := db.GetLease(context.Background(), id)
	leaseRow.SnapshotBuildID = row.BuildID
	_ = db.UpsertLease(context.Background(), leaseRow)
	resp, body := doReq(t, "DELETE", ts.URL+"/api/named-snapshots/cold", "token-a", nil)
	if resp.StatusCode != http.StatusConflict || body["code"] != "snapshot_in_use" {
		t.Fatalf("delete in use: status %d (%v), want 409 snapshot_in_use", resp.StatusCode, body)
	}
	resp, _ = doReq(t, "DELETE", ts.URL+"/api/named-snapshots/cold?force=1", "token-a", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("force delete: status %d, want 204", resp.StatusCode)
	}
}

// TestNamedSnapshotOwnerIsolation: another owner's name is invisible.
func TestNamedSnapshotOwnerIsolation(t *testing.T) {
	ts, svc, db, _ := newTestServerWithService(t)
	svc.cfg.TemplateStoragePath = t.TempDir()
	seedImage(t, db, "py-base", 2048)
	id := createLiveLease(t, ts.URL, "token-a", nil)
	saveSnapshotOK(t, ts.URL, "token-a", id, map[string]any{"name": "warm"})

	for _, route := range []string{"/api/named-snapshots/warm", "/api/named-snapshots/warm@1"} {
		resp, _ := doReq(t, "GET", ts.URL+route, "token-b", nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("GET %s as token-b: status %d, want 404", route, resp.StatusCode)
		}
	}
	resp, _ := doReq(t, "DELETE", ts.URL+"/api/named-snapshots/warm", "token-b", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("DELETE as token-b: status %d, want 404", resp.StatusCode)
	}
	// token-b's own list is empty.
	resp, list := doReq(t, "GET", ts.URL+"/api/named-snapshots", "token-b", nil)
	if resp.StatusCode != http.StatusOK || len(list["snapshots"].([]any)) != 0 {
		t.Fatalf("token-b list = %v, want empty", list)
	}
}

// TestNamedSnapshotGCKeepsBuildAndParentChain: a named snapshot's build
// and its ancestors are GC roots.
func TestNamedSnapshotGCKeepsBuildAndParentChain(t *testing.T) {
	ts, svc, db, _ := newTestServerWithService(t)
	svc.cfg.TemplateStoragePath = t.TempDir()
	svc.cfg.PoolSize = 0
	seedImage(t, db, "py-base", 2048)
	id := createLiveLease(t, ts.URL, "token-a", nil)
	saveSnapshotOK(t, ts.URL, "token-a", id, map[string]any{"name": "warm"})

	row, err := db.GetNamedSnapshotLatest(context.Background(), "consumer-a", "warm")
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	kept, err := svc.keptBuilds(t.Context())
	if err != nil {
		t.Fatalf("keptBuilds: %v", err)
	}
	if !kept[row.BuildID] {
		t.Fatalf("named build %s not kept", row.BuildID)
	}
	root, _, err := svc.namedSnapshotBuildRoot(t.Context(), row.BuildID)
	if err != nil {
		t.Fatalf("root: %v", err)
	}
	if !kept[root] {
		t.Fatalf("parent chain root %s not kept", root)
	}
	roots, err := svc.orphanRoots(t.Context())
	if err != nil {
		t.Fatalf("orphanRoots: %v", err)
	}
	if !roots[row.BuildID] {
		t.Fatalf("named build %s not an orphan root", row.BuildID)
	}
}

// TestNamedSnapshotLeaseIDFile: the create writes
// /run/spoond/lease-id (0644) with the lease id.
func TestNamedSnapshotLeaseIDFile(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	svc.cfg.TemplateStoragePath = t.TempDir()
	seedImage(t, db, "py-base", 2048)
	id := createLiveLease(t, ts.URL, "token-a", nil)
	sandbox := svc.store.leases[id].SandboxID
	data, err := sub.Fake.ReadFile(t.Context(), sandbox, leaseIDPath, 1024)
	if err != nil {
		t.Fatalf("read lease-id: %v", err)
	}
	if strings.TrimSpace(string(data)) != id {
		t.Fatalf("lease-id = %q, want %q", data, id)
	}
	info, err := sub.Fake.Stat(t.Context(), sandbox, leaseIDPath)
	if err != nil {
		t.Fatalf("stat lease-id: %v", err)
	}
	if info.Mode.Perm() != 0o644 {
		t.Fatalf("lease-id mode = %o, want 644", info.Mode.Perm())
	}
}

// TestNamedSnapshotLastSaveMarker: the source writes
// /run/spoond/last-save after the checkpoint with the version JSON.
func TestNamedSnapshotLastSaveMarker(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	svc.cfg.TemplateStoragePath = t.TempDir()
	seedImage(t, db, "py-base", 2048)
	id := createLiveLease(t, ts.URL, "token-a", nil)
	sandbox := svc.store.leases[id].SandboxID
	saveSnapshotOK(t, ts.URL, "token-a", id, map[string]any{"name": "warm", "idempotency_key": "k1"})

	data, err := sub.Fake.ReadFile(t.Context(), sandbox, lastSavePath, 4096)
	if err != nil {
		t.Fatalf("read last-save: %v", err)
	}
	var marker struct {
		Name           string `json:"name"`
		Version        int64  `json:"version"`
		BuildID        string `json:"build_id"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if err := json.Unmarshal(data, &marker); err != nil {
		t.Fatalf("last-save JSON: %v (%s)", err, data)
	}
	if marker.Name != "warm" || marker.Version != 1 || marker.IdempotencyKey != "k1" {
		t.Fatalf("last-save = %+v", marker)
	}
	if _, err := sub.Fake.Stat(t.Context(), sandbox, lastSavePath+".tmp"); err == nil {
		t.Fatal("last-save.tmp left behind; the rename should have moved it")
	}
}

// TestNamedSnapshotMetrics: the gauges track versions and bytes.
func TestNamedSnapshotMetrics(t *testing.T) {
	ts, svc, db, _ := newTestServerWithService(t)
	svc.cfg.TemplateStoragePath = t.TempDir()
	seedImage(t, db, "py-base", 2048)
	id := createLiveLease(t, ts.URL, "token-a", nil)
	saveSnapshotOK(t, ts.URL, "token-a", id, map[string]any{"name": "warm"})
	saveSnapshotOK(t, ts.URL, "token-a", id, map[string]any{"name": "warm"})
	versions, _, err := db.NamedSnapshotMetrics(context.Background())
	if err != nil {
		t.Fatalf("metrics: %v", err)
	}
	if versions != 2 {
		t.Fatalf("versions = %d, want 2", versions)
	}
}

// TestNamedSnapshotListPrefix: ?prefix= narrows the list.
func TestNamedSnapshotListPrefix(t *testing.T) {
	ts, svc, db, _ := newTestServerWithService(t)
	svc.cfg.TemplateStoragePath = t.TempDir()
	seedImage(t, db, "py-base", 2048)
	id := createLiveLease(t, ts.URL, "token-a", nil)
	saveSnapshotOK(t, ts.URL, "token-a", id, map[string]any{"name": "spoond/warm"})
	saveSnapshotOK(t, ts.URL, "token-a", id, map[string]any{"name": "other"})

	resp, list := doReq(t, "GET", ts.URL+"/api/named-snapshots?prefix=spoond/", "token-a", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: status %d: %v", resp.StatusCode, list)
	}
	snaps := list["snapshots"].([]any)
	if len(snaps) != 1 || snaps[0].(map[string]any)["name"] != "spoond/warm" {
		t.Fatalf("prefix list = %v, want only spoond/warm", snaps)
	}
}

// TestNamedSnapshotStale: a rebuilt image makes the version stale.
func TestNamedSnapshotStale(t *testing.T) {
	ts, svc, db, _ := newTestServerWithService(t)
	svc.cfg.TemplateStoragePath = t.TempDir()
	img := seedImage(t, db, "py-base", 2048)
	id := createLiveLease(t, ts.URL, "token-a", nil)
	saveSnapshotOK(t, ts.URL, "token-a", id, map[string]any{"name": "warm"})

	// The image's current build moves to a fresh build.
	newBuild := e2b.NewUUID()
	now := time.Now()
	if err := db.InsertBuild(context.Background(), store.BuildRow{
		BuildID: newBuild, Kind: "template", TemplateID: img.TemplateID, Image: "py-base",
		State: "ready", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("insert new build: %v", err)
	}
	img.CurrentBuildID = newBuild
	if err := db.UpsertImage(context.Background(), img); err != nil {
		t.Fatalf("upsert image: %v", err)
	}
	resp, detail := doReq(t, "GET", ts.URL+"/api/named-snapshots/warm", "token-a", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("show: status %d: %v", resp.StatusCode, detail)
	}
	if !detail["stale"].(bool) {
		t.Fatalf("stale = %v, want true after the image was rebuilt", detail)
	}
}

// TestNamedSnapshotDeleteCollidingPrefix: deleting a name that only
// shares a prefix with an existing one is 404, not 204-and-noop, and an
// in-use colliding name is not consulted (BLOCKER fix).
func TestNamedSnapshotDeleteCollidingPrefix(t *testing.T) {
	ts, svc, db, _ := newTestServerWithService(t)
	svc.cfg.TemplateStoragePath = t.TempDir()
	seedImage(t, db, "py-base", 2048)
	id := createLiveLease(t, ts.URL, "token-a", nil)
	saveSnapshotOK(t, ts.URL, "token-a", id, map[string]any{"name": "warmup"})

	// Only "warmup" exists. Deleting "warm" must not see it.
	resp, body := doReq(t, "DELETE", ts.URL+"/api/named-snapshots/warm", "token-a", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("delete warm with only warmup present: status %d (%v), want 404", resp.StatusCode, body)
	}
	if rows, _ := db.ListNamedSnapshotsExact(context.Background(), "consumer-a", "warmup"); len(rows) != 1 {
		t.Fatalf("warmup rows = %d, want 1 (untouched)", len(rows))
	}

	// Mark warmup in use, then delete "warm": 404, never 409 from warmup's
	// versions.
	row, _ := db.GetNamedSnapshotLatest(context.Background(), "consumer-a", "warmup")
	leaseRow, _ := db.GetLease(context.Background(), id)
	leaseRow.SnapshotBuildID = row.BuildID
	_ = db.UpsertLease(context.Background(), leaseRow)
	resp, body = doReq(t, "DELETE", ts.URL+"/api/named-snapshots/warm", "token-a", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("delete warm with warmup in use: status %d (%v), want 404", resp.StatusCode, body)
	}
	// The real name still deletes (it is in use, so 409, then forced 204).
	resp, body = doReq(t, "DELETE", ts.URL+"/api/named-snapshots/warmup", "token-a", nil)
	if resp.StatusCode != http.StatusConflict || body["code"] != "snapshot_in_use" {
		t.Fatalf("delete warmup in use: status %d (%v), want 409 snapshot_in_use", resp.StatusCode, body)
	}
}

// TestNamedSnapshotKeptBudget: named snapshot bytes count toward the
// owner's max_kept_bytes; a save past it is 409 kept_budget and stores
// nothing (the untested limit from the task's test list).
func TestNamedSnapshotKeptBudget(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	root := t.TempDir()
	svc.cfg.TemplateStoragePath = root
	seedImage(t, db, "py-base", 2048)

	// Each checkpoint writes a real build directory so size_bytes is
	// nonzero and measurable.
	sub.checkpointFn = func(ctx context.Context, sandboxID string) (string, substrate.BuildRefs, error) {
		id := e2b.NewUUID()
		if _, err := writeBuildDir(filepath.Join(root, id), 4096, 8192); err != nil {
			return "", substrate.BuildRefs{}, err
		}
		return id, substrate.BuildRefs{}, nil
	}

	// Measure one build's allocated size on this filesystem, then allow
	// only one build's worth: the first save fits, the second does not.
	probe := filepath.Join(t.TempDir(), "probe")
	one, err := writeBuildDir(probe, 4096, 8192)
	if err != nil || one <= 0 {
		t.Fatalf("probe build size %d: %v", one, err)
	}
	ids, err := identity.NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	svc.SetIdentities(ids)
	u, err := ids.AddUser("keeper", identity.KindPerson, []string{"SHA256:fp-k"}, "keeper-tok")
	if err != nil {
		t.Fatal(err)
	}
	if err := ids.SetQuota(u.ID, 0, 0, 0, 0, one+one/2); err != nil {
		t.Fatal(err)
	}

	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "keeper-tok",
		map[string]any{"image": "py-base", "ttl": 300, "persistent": true})
	id := create["id"].(string)

	// First save lands under the budget.
	if v := saveSnapshotOK(t, ts.URL, "keeper-tok", id, map[string]any{"name": "warm"}); v != 1 {
		t.Fatalf("first save version = %d, want 1", v)
	}
	// The next save would pass it: 409 kept_budget, no new version.
	resp, body := saveSnapshot(t, ts.URL, "keeper-tok", id, map[string]any{"name": "warm"})
	if resp.StatusCode != http.StatusConflict || body["code"] != "kept_budget" {
		t.Fatalf("over-budget save: status %d (%v), want 409 kept_budget", resp.StatusCode, body)
	}
	rows, err := db.ListNamedSnapshotsExact(context.Background(), u.ID, "warm")
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows after over-budget save = %+v (%v), want just v1", rows, err)
	}
}

// TestNamedSnapshotKeysErrorsHaveCodes: every named-snapshot route's
// error body carries a machine-readable code beside error (A2).
func TestNamedSnapshotKeysErrorsHaveCodes(t *testing.T) {
	ts, svc, db, _ := newTestServerWithService(t)
	svc.cfg.TemplateStoragePath = t.TempDir()
	seedImage(t, db, "py-base", 2048)
	id := createLiveLease(t, ts.URL, "token-a", nil)
	saveSnapshotOK(t, ts.URL, "token-a", id, map[string]any{"name": "warm"})
	_ = id

	cases := []struct {
		method, path string
		body         any
		wantStatus   int
		wantCode     string
	}{
		{"GET", "/api/named-snapshots/nope", nil, http.StatusNotFound, "not_found"},
		{"DELETE", "/api/named-snapshots/nope", nil, http.StatusNotFound, "not_found"},
		{"GET", "/api/named-snapshots/warm@99", nil, http.StatusNotFound, "not_found"},
		{"DELETE", "/api/named-snapshots/warm@99", nil, http.StatusNotFound, "not_found"},
		{"PUT", "/api/named-snapshots/nope", map[string]any{"keep": 2}, http.StatusNotFound, "not_found"},
		{"GET", "/api/named-snapshots/warm@0", nil, http.StatusBadRequest, "bad_request"},
	}
	for _, c := range cases {
		resp, body := doReq(t, c.method, ts.URL+c.path, "token-a", c.body)
		if resp.StatusCode != c.wantStatus {
			t.Fatalf("%s %s: status %d (%v), want %d", c.method, c.path, resp.StatusCode, body, c.wantStatus)
		}
		if body["code"] != c.wantCode {
			t.Fatalf("%s %s: code = %v (%v), want %q", c.method, c.path, body["code"], body, c.wantCode)
		}
	}
}

// TestNamedSnapshotEventDetail: a save emits a snapshot_saved event with
// the documented detail.
func TestNamedSnapshotEventDetail(t *testing.T) {
	ts, svc, db, _ := newTestServerWithService(t)
	svc.cfg.TemplateStoragePath = t.TempDir()
	seedImage(t, db, "py-base", 2048)
	id := createLiveLease(t, ts.URL, "token-a", nil)
	saveSnapshotOK(t, ts.URL, "token-a", id, map[string]any{"name": "warm"})

	bus := svc.bus
	bus.mu.Lock()
	defer bus.mu.Unlock()
	found := false
	for _, ev := range bus.ring {
		if ev.Type == LeaseSnapshotSaved && ev.LeaseID == id {
			found = true
			if !strings.HasPrefix(ev.Detail, "saved as warm@1 · ") {
				t.Fatalf("snapshot_saved detail = %q, want \"saved as warm@1 · ...\"", ev.Detail)
			}
			if !strings.Contains(ev.Detail, " · ") {
				t.Fatalf("snapshot_saved detail = %q, want a size and duration", ev.Detail)
			}
		}
	}
	if !found {
		t.Fatal("no snapshot_saved event on the bus")
	}
}

// TestNamedSnapshotLeaseIDFileAfterFork: a fork writes
// /run/spoond/lease-id for the child too.
func TestNamedSnapshotLeaseIDFileAfterFork(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	svc.cfg.TemplateStoragePath = t.TempDir()
	seedImage(t, db, "py-base", 2048)
	id := createLiveLease(t, ts.URL, "token-a", map[string]any{"image": "py-base", "ttl": 300, "persistent": true})

	resp, out := doReq(t, "POST", ts.URL+"/api/leases/"+id+"/fork", "token-a", map[string]any{"count": 1})
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		t.Fatalf("fork: status %d: %v", resp.StatusCode, out)
	}
	// Find the child lease and read its lease-id file.
	list, err := db.ListLeases(context.Background())
	if err != nil {
		t.Fatalf("list leases: %v", err)
	}
	var child *store.LeaseRow
	for i := range list {
		if list[i].ID != id && list[i].Owner == "consumer-a" {
			child = &list[i]
		}
	}
	if child == nil {
		t.Fatalf("fork produced no child lease: %v", list)
	}
	sb, err := db.GetSandboxByLease(context.Background(), child.ID)
	if err != nil {
		t.Fatalf("child sandbox: %v", err)
	}
	data, err := sub.Fake.ReadFile(t.Context(), sb.SandboxID, leaseIDPath, 1024)
	if err != nil {
		t.Fatalf("read child lease-id: %v", err)
	}
	if strings.TrimSpace(string(data)) != child.ID {
		t.Fatalf("child lease-id = %q, want %q", data, child.ID)
	}

	// A resume rewrites it too.
	if resp, out := doReq(t, "POST", ts.URL+"/api/leases/"+id+"/suspend", "token-a", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("suspend: %d %v", resp.StatusCode, out)
	}
	if resp, out := doReq(t, "POST", ts.URL+"/api/leases/"+id+"/resume", "token-a", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("resume: %d %v", resp.StatusCode, out)
	}
	sb, _ = db.GetSandboxByLease(context.Background(), id)
	data, err = sub.Fake.ReadFile(t.Context(), sb.SandboxID, leaseIDPath, 1024)
	if err != nil {
		t.Fatalf("read resumed lease-id: %v", err)
	}
	if strings.TrimSpace(string(data)) != id {
		t.Fatalf("resumed lease-id = %q, want %q", data, id)
	}
}

// TestNamedSnapshotLingeringExecSecretScrubbed: an exec-time secret file
// a failed cleanup left behind is scrubbed before the checkpoint, so it
// is never captured (NOTE fix).
func TestNamedSnapshotLingeringExecSecretScrubbed(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	svc.cfg.TemplateStoragePath = t.TempDir()
	seedImage(t, db, "py-base", 2048)
	id := createLiveLease(t, ts.URL, "token-a", nil)
	sandbox := svc.store.leases[id].SandboxID

	// A job staged an exec-time secret and never cleaned it up.
	if err := sub.Fake.WriteFile(t.Context(), sandbox, "/run/secrets/EXECTOK", []byte("v"), 0o600); err != nil {
		t.Fatalf("write lingering secret: %v", err)
	}
	svc.secretsMu.Lock()
	svc.stagedJobSecrets["job-1"] = []string{"EXECTOK"}
	svc.jobSecretLeases["job-1"] = id
	svc.secretsMu.Unlock()
	defer svc.takeJobSecretNames("job-1")

	var atCheckpoint error
	sub.checkpointFn = func(ctx context.Context, sandboxID string) (string, substrate.BuildRefs, error) {
		_, atCheckpoint = sub.Fake.ReadFile(ctx, sandboxID, "/run/secrets/EXECTOK", 1024)
		return e2b.NewUUID(), substrate.BuildRefs{}, nil
	}
	saveSnapshotOK(t, ts.URL, "token-a", id, map[string]any{"name": "warm"})
	if atCheckpoint == nil {
		t.Fatal("lingering exec-time secret was still present at checkpoint time; want it scrubbed")
	}
}
