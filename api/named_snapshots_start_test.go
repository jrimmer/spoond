package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/jrimmer/spoond/v2/identity"
	"github.com/jrimmer/spoond/v2/substrate"
)

// createFromSnapshot creates a lease over the API with the given body
// (which must carry "snapshot") and asserts 201.
func createFromSnapshot(t *testing.T, ts, token string, body map[string]any) map[string]any {
	t.Helper()
	resp, out := doReq(t, "POST", ts+"/api/sandboxes", token, body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create from snapshot: status %d: %v", resp.StatusCode, out)
	}
	return out
}

// readStartedFrom parses /run/spoond/started-from out of the fake.
func readStartedFrom(t *testing.T, sub *testSub, sandboxID string) map[string]any {
	t.Helper()
	data, err := sub.Fake.ReadFile(t.Context(), sandboxID, startedFromPath, 4096)
	if err != nil {
		t.Fatalf("read %s: %v", startedFromPath, err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("started-from JSON: %v (%s)", err, data)
	}
	return out
}

// TestSnapshotStartByNameAndVersion: a lease can start from a named
// snapshot by latest name or by pinned @v, and the version's marker and
// build link are recorded. The create answers with the snapshot object
// (A3) and marks the lease as started from the version.
func TestSnapshotStartByNameAndVersion(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	svc.cfg.TemplateStoragePath = t.TempDir()
	seedImage(t, db, "py-base", 2048)
	src := createLiveLease(t, ts.URL, "token-a", nil)

	v1 := saveSnapshotOK(t, ts.URL, "token-a", src, map[string]any{"name": "warm"})
	v2 := saveSnapshotOK(t, ts.URL, "token-a", src, map[string]any{"name": "warm"})
	if v1 != 1 || v2 != 2 {
		t.Fatalf("versions = %d, %d", v1, v2)
	}

	// By latest name: resolves v2.
	out := createFromSnapshot(t, ts.URL, "token-a", map[string]any{"snapshot": "warm", "ttl": 300})
	if out["id"] == src {
		t.Fatalf("snapshot create reused the source id")
	}
	view, ok := out["snapshot"].(map[string]any)
	if !ok {
		t.Fatalf("create body has no snapshot object: %v", out)
	}
	if view["name"] != "warm" || view["version"].(float64) != 2 {
		t.Fatalf("snapshot object = %v, want warm@2", view)
	}
	if out["generation"].(float64) != 1 {
		t.Fatalf("generation = %v, want 1", out["generation"])
	}
	leaseID := out["id"].(string)
	l := svc.store.leases[leaseID]
	if l == nil {
		t.Fatalf("lease %s not registered", leaseID)
	}
	row, err := db.GetNamedSnapshot(context.Background(), "consumer-a", "warm", 2)
	if err != nil {
		t.Fatalf("v2: %v", err)
	}
	if l.SnapshotBuildID != row.BuildID {
		t.Fatalf("snapshot_build_id = %q, want %q", l.SnapshotBuildID, row.BuildID)
	}
	// The build row carries the link too.
	stored, err := db.GetLease(context.Background(), leaseID)
	if err != nil {
		t.Fatalf("stored lease: %v", err)
	}
	if stored.SnapshotBuildID != row.BuildID {
		t.Fatalf("stored snapshot_build_id = %q, want %q", stored.SnapshotBuildID, row.BuildID)
	}

	// Copy-side markers (A4): lease-id, generation and started-from exist
	// and carry the new lease's identity.
	sb, err := db.GetSandboxByLease(context.Background(), leaseID)
	if err != nil {
		t.Fatalf("new sandbox: %v", err)
	}
	data, err := sub.Fake.ReadFile(t.Context(), sb.SandboxID, leaseIDPath, 1024)
	if err != nil || strings.TrimSpace(string(data)) != leaseID {
		t.Fatalf("lease-id = %q (%v), want %q", data, err, leaseID)
	}
	gen := readGeneration(t, sub, sb.SandboxID)
	if gen != "1\n" {
		t.Fatalf("generation file = %q, want 1", gen)
	}
	marker := readStartedFrom(t, sub, sb.SandboxID)
	if marker["name"] != "warm" || marker["version"].(float64) != 2 || marker["build_id"] != row.BuildID {
		t.Fatalf("started-from = %v", marker)
	}
	if _, err := sub.Fake.Stat(t.Context(), sb.SandboxID, startedFromPath+".tmp"); err == nil {
		t.Fatal("started-from.tmp left behind; the rename should have moved it")
	}

	// The lease detail GET carries the same snapshot object.
	resp, detail := doReq(t, "GET", ts.URL+"/api/leases/"+leaseID, "token-a", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("detail: %d %v", resp.StatusCode, detail)
	}
	dview, ok := detail["snapshot"].(map[string]any)
	if !ok || dview["version"].(float64) != 2 || dview["build_id"] != row.BuildID {
		t.Fatalf("detail snapshot = %v, want warm@2 %s", detail["snapshot"], row.BuildID)
	}

	// By pinned @v: resolves v1 and writes the v1 build link.
	out = createFromSnapshot(t, ts.URL, "token-a", map[string]any{"snapshot": "warm@1"})
	if out["snapshot"].(map[string]any)["version"].(float64) != 1 {
		t.Fatalf("pinned create = %v, want warm@1", out["snapshot"])
	}
	row1, _ := db.GetNamedSnapshot(context.Background(), "consumer-a", "warm", 1)
	if svc.store.leases[out["id"].(string)].SnapshotBuildID != row1.BuildID {
		t.Fatalf("pinned lease build = %q, want %q", svc.store.leases[out["id"].(string)].SnapshotBuildID, row1.BuildID)
	}
}

// TestSnapshotStartImageMismatch: an explicit image that is not the
// snapshot's image is 400 image_mismatch; a matching image is accepted.
// An unknown name/version is 404 not_found.
func TestSnapshotStartImageMismatch(t *testing.T) {
	ts, svc, db, _ := newTestServerWithService(t)
	svc.cfg.TemplateStoragePath = t.TempDir()
	seedImage(t, db, "py-base", 2048)
	seedImage(t, db, "go-base", 4096)
	id := createLiveLease(t, ts.URL, "token-a", nil)
	saveSnapshotOK(t, ts.URL, "token-a", id, map[string]any{"name": "warm"})

	resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a",
		map[string]any{"snapshot": "warm", "image": "go-base"})
	if resp.StatusCode != http.StatusBadRequest || body["code"] != "image_mismatch" {
		t.Fatalf("mismatched image: status %d (%v), want 400 image_mismatch", resp.StatusCode, body)
	}
	// A matching image is fine.
	out := createFromSnapshot(t, ts.URL, "token-a", map[string]any{"snapshot": "warm", "image": "py-base"})
	if out["image"] != "py-base" {
		t.Fatalf("image = %v, want py-base", out["image"])
	}

	for _, ref := range []string{"nope", "warm@99"} {
		resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"snapshot": ref})
		if resp.StatusCode != http.StatusNotFound || body["code"] != "not_found" {
			t.Fatalf("start %s: status %d (%v), want 404 not_found", ref, resp.StatusCode, body)
		}
	}
}

// TestSnapshotStartMemoryQuota: the snapshot's memory_mb, not the
// image's, is the admission and quota charge.
func TestSnapshotStartMemoryQuota(t *testing.T) {
	ts, svc, db, _ := newTestServerWithService(t)
	svc.cfg.TemplateStoragePath = t.TempDir()
	seedImage(t, db, "py-base", 2048)

	ids, err := identity.NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	svc.SetIdentities(ids)
	u, err := ids.AddUser("m", identity.KindPerson, []string{"SHA256:fp-m"}, "m-tok")
	if err != nil {
		t.Fatal(err)
	}

	// Create the source and save a snapshot (owner-scoped to m).
	srcID := createLiveLease(t, ts.URL, "m-tok", nil)
	saveSnapshotOK(t, ts.URL, "m-tok", srcID, map[string]any{"name": "warm"})
	resp, _ := doReq(t, "DELETE", ts.URL+"/api/leases/"+srcID, "m-tok", nil)
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		t.Fatalf("release source: status %d", resp.StatusCode)
	}

	// Change the image's memory to 4096 after saving: the snapshot keeps
	// the 2048 it was saved at. A max_mib of 3000 refuses a plain create
	// but admits the snapshot start.
	img, err := db.GetImage(context.Background(), "py-base")
	if err != nil {
		t.Fatalf("image: %v", err)
	}
	img.MemoryMB = 4096
	if err := db.UpsertImage(context.Background(), img); err != nil {
		t.Fatalf("bump image memory: %v", err)
	}
	if err := ids.SetQuota(u.ID, 0, 0, 0, 3000, 0); err != nil {
		t.Fatal(err)
	}

	// A plain create of py-base (now 4096 MiB) is over max_mib.
	resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes", "m-tok", map[string]any{"image": "py-base"})
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("plain create: status %d (%v), want 429", resp.StatusCode, body)
	}
	// A create from the 2048 MiB snapshot fits.
	out := createFromSnapshot(t, ts.URL, "m-tok", map[string]any{"snapshot": "warm"})
	newID := out["id"].(string)
	if got := svc.store.leases[newID].MemoryMB; got != 2048 {
		t.Fatalf("lease memory_mb = %d, want the snapshot's 2048", got)
	}
	if got := svc.usedMiB(u.ID); got != 2048 {
		t.Fatalf("usedMiB = %d, want 2048", got)
	}
}

// TestSnapshotStartSecrets: the request's create-time secrets are staged
// on the new lease, and the lease-id marker names the new lease.
func TestSnapshotStartSecrets(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	svc.cfg.TemplateStoragePath = t.TempDir()
	seedImage(t, db, "py-base", 2048)
	src := createLiveLease(t, ts.URL, "token-a", nil)
	saveSnapshotOK(t, ts.URL, "token-a", src, map[string]any{"name": "warm"})

	out := createFromSnapshot(t, ts.URL, "token-a", map[string]any{
		"snapshot": "warm",
		"secrets":  map[string]string{"TOKEN": "s3cret"},
	})
	newID := out["id"].(string)
	sb, err := db.GetSandboxByLease(context.Background(), newID)
	if err != nil {
		t.Fatalf("new sandbox: %v", err)
	}
	data, err := sub.Fake.ReadFile(t.Context(), sb.SandboxID, "/run/secrets/TOKEN", 1024)
	if err != nil || string(data) != "s3cret" {
		t.Fatalf("/run/secrets/TOKEN = %q (%v), want s3cret", data, err)
	}
	if got := svc.createSecretsFor(newID); got["TOKEN"] != "s3cret" {
		t.Fatalf("create secrets not remembered: %v", got)
	}
}

// TestSnapshotStartCannotStart: a substrate create failure that means the
// snapshot's build cannot start on this host answers 409 cannot_start;
// a capacity failure keeps its usual 503 and a generic failure its 500.
func TestSnapshotStartCannotStart(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	svc.cfg.TemplateStoragePath = t.TempDir()
	seedImage(t, db, "py-base", 2048)
	src := createLiveLease(t, ts.URL, "token-a", nil)
	saveSnapshotOK(t, ts.URL, "token-a", src, map[string]any{"name": "warm"})

	// The orchestrator does not know the build: missing build files.
	sub.createFn = func(ctx context.Context, req substrate.CreateRequest) (substrate.Sandbox, error) {
		return substrate.Sandbox{}, fmt.Errorf("e2b: create %s: %w", req.BuildID, substrate.ErrNotFound)
	}
	t.Cleanup(func() { sub.createFn = nil })
	resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"snapshot": "warm"})
	if resp.StatusCode != http.StatusConflict || body["code"] != "cannot_start" {
		t.Fatalf("missing build: status %d (%v), want 409 cannot_start", resp.StatusCode, body)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "cannot start on this host") || !strings.Contains(msg, "save it again") {
		t.Fatalf("cannot_start error = %q", msg)
	}

	// An incompatible envd also cannot start.
	sub.createFn = func(ctx context.Context, req substrate.CreateRequest) (substrate.Sandbox, error) {
		return substrate.Sandbox{}, fmt.Errorf("e2b: create %s: envd not healthy within 30s: connect: connection refused", req.BuildID)
	}
	resp, body = doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"snapshot": "warm"})
	if resp.StatusCode != http.StatusConflict || body["code"] != "cannot_start" {
		t.Fatalf("incompatible envd: status %d (%v), want 409 cannot_start", resp.StatusCode, body)
	}

	// Capacity keeps its current 503 handling, not cannot_start.
	sub.createFn = func(ctx context.Context, req substrate.CreateRequest) (substrate.Sandbox, error) {
		return substrate.Sandbox{}, fmt.Errorf("%w: node full", substrate.ErrCapacity)
	}
	resp, body = doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"snapshot": "warm"})
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("capacity: status %d (%v), want 503", resp.StatusCode, body)
	}
	if body["code"] == "cannot_start" {
		t.Fatalf("capacity misclassified as cannot_start: %v", body)
	}

	// A generic transport failure stays a 500.
	sub.createFn = func(ctx context.Context, req substrate.CreateRequest) (substrate.Sandbox, error) {
		return substrate.Sandbox{}, fmt.Errorf("connection reset by peer")
	}
	resp, _ = doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"snapshot": "warm"})
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("generic failure: status %d, want 500", resp.StatusCode)
	}
}

// TestSnapshotStartCopySideBeforeFirstExec: the lease-id, generation and
// started-from markers exist before the first exec (the integrity probe)
// of the create, and before the create answers.
func TestSnapshotStartCopySideBeforeFirstExec(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	svc.cfg.TemplateStoragePath = t.TempDir()
	seedImage(t, db, "py-base", 2048)
	src := createLiveLease(t, ts.URL, "token-a", nil)
	saveSnapshotOK(t, ts.URL, "token-a", src, map[string]any{"name": "warm"})

	// At the first exec (the probe), the copy-side markers must already
	// be in the guest.
	type seen struct {
		leaseID, generation, startedFrom string
	}
	var atExec *seen
	sub.execBefore = func(sandboxID string, req substrate.ExecRequest) {
		if atExec != nil || len(req.Args) != 3 || req.Args[2] != integrityProbe {
			return
		}
		s := &seen{}
		if data, err := sub.Fake.ReadFile(context.Background(), sandboxID, leaseIDPath, 1024); err == nil {
			s.leaseID = strings.TrimSpace(string(data))
		}
		if data, err := sub.Fake.ReadFile(context.Background(), sandboxID, generationPath, 1024); err == nil {
			s.generation = strings.TrimSpace(string(data))
		}
		if data, err := sub.Fake.ReadFile(context.Background(), sandboxID, startedFromPath, 4096); err == nil {
			s.startedFrom = string(data)
		}
		atExec = s
	}
	t.Cleanup(func() { sub.execBefore = nil })

	out := createFromSnapshot(t, ts.URL, "token-a", map[string]any{"snapshot": "warm"})
	newID := out["id"].(string)
	if atExec == nil {
		t.Fatal("no exec observed during the create")
	}
	if atExec.leaseID != newID {
		t.Fatalf("lease-id at first exec = %q, want %q (written after the exec)", atExec.leaseID, newID)
	}
	if atExec.generation != "1" {
		t.Fatalf("generation at first exec = %q, want 1", atExec.generation)
	}
	var marker map[string]any
	if err := json.Unmarshal([]byte(atExec.startedFrom), &marker); err != nil {
		t.Fatalf("started-from at first exec: %v (%q)", err, atExec.startedFrom)
	}
	if marker["name"] != "warm" || marker["version"].(float64) != 1 {
		t.Fatalf("started-from at first exec = %v", marker)
	}
}

// TestSnapshotStartCreatedEvent: the created event says the lease
// started from the snapshot version.
func TestSnapshotStartCreatedEvent(t *testing.T) {
	ts, svc, db, _ := newTestServerWithService(t)
	svc.cfg.TemplateStoragePath = t.TempDir()
	seedImage(t, db, "py-base", 2048)
	src := createLiveLease(t, ts.URL, "token-a", nil)
	saveSnapshotOK(t, ts.URL, "token-a", src, map[string]any{"name": "warm"})
	out := createFromSnapshot(t, ts.URL, "token-a", map[string]any{"snapshot": "warm"})
	newID := out["id"].(string)

	bus := svc.bus
	bus.mu.Lock()
	defer bus.mu.Unlock()
	for _, ev := range bus.ring {
		if ev.Type == LeaseCreated && ev.LeaseID == newID {
			if !strings.HasPrefix(ev.Detail, "started from snapshot warm@1 in ") {
				t.Fatalf("created detail = %q", ev.Detail)
			}
			return
		}
	}
	t.Fatal("no created event for the snapshot-started lease")
}

// TestSnapshotStartBypassesWarmPool: a snapshot start never takes a warm
// pooled sandbox (the pool holds the image's current build).
func TestSnapshotStartBypassesWarmPool(t *testing.T) {
	ts, svc, db, _ := newTestServerWithService(t)
	svc.cfg.TemplateStoragePath = t.TempDir()
	seedImage(t, db, "py-base", 2048)
	src := createLiveLease(t, ts.URL, "token-a", nil)
	saveSnapshotOK(t, ts.URL, "token-a", src, map[string]any{"name": "warm"})

	svc.cfg.PoolSize = 2
	svc.refillPool(t.Context())
	before := len(svc.store.pool["py-base"])
	out := createFromSnapshot(t, ts.URL, "token-a", map[string]any{"snapshot": "warm"})
	if svc.store.leases[out["id"].(string)].pooled {
		t.Fatal("snapshot-started lease was served from the warm pool")
	}
	if got := len(svc.store.pool["py-base"]); got != before {
		t.Fatalf("pool size = %d, want %d (untouched)", got, before)
	}
}

// TestSnapshotStartLiveLeaseRetentionEndToEnd: a version saved from a
// lease that a running lease started from survives retention, and is
// dropped once that lease is released (S5).
func TestSnapshotStartLiveLeaseRetentionEndToEnd(t *testing.T) {
	ts, svc, db, _ := newTestServerWithService(t)
	svc.cfg.TemplateStoragePath = t.TempDir()
	seedImage(t, db, "py-base", 2048)
	src := createLiveLease(t, ts.URL, "token-a", nil)

	// First save pins keep=2 on the name.
	v1 := saveSnapshotOK(t, ts.URL, "token-a", src, map[string]any{"name": "warm", "keep": 2})
	if v1 != 1 {
		t.Fatalf("v1 = %d", v1)
	}
	// A second lease starts from v1.
	out := createFromSnapshot(t, ts.URL, "token-a", map[string]any{"snapshot": "warm@1", "persistent": true})
	user := out["id"].(string)

	// Save v2..v4: keep=2 would drop v1 and v2, but the lease runs from
	// v1.
	saveSnapshotOK(t, ts.URL, "token-a", src, map[string]any{"name": "warm"})
	saveSnapshotOK(t, ts.URL, "token-a", src, map[string]any{"name": "warm"})
	saveSnapshotOK(t, ts.URL, "token-a", src, map[string]any{"name": "warm"})
	if _, err := db.GetNamedSnapshot(context.Background(), "consumer-a", "warm", 1); err != nil {
		t.Fatalf("v1 was pruned while a live lease used it: %v", err)
	}
	// v2 is beyond keep and unused: pruned.
	if _, err := db.GetNamedSnapshot(context.Background(), "consumer-a", "warm", 2); err == nil {
		t.Fatal("v2 should have been pruned")
	}
	// in_use reflects the live lease on v1.
	resp, detail := doReq(t, "GET", ts.URL+"/api/named-snapshots/warm@1", "token-a", nil)
	if resp.StatusCode != http.StatusOK || detail["in_use"].(float64) != 1 {
		t.Fatalf("v1 detail = %d %v, want in_use 1", resp.StatusCode, detail)
	}
	// Delete-in-use is 409 snapshot_in_use, not 204.
	resp, body := doReq(t, "DELETE", ts.URL+"/api/named-snapshots/warm@1", "token-a", nil)
	if resp.StatusCode != http.StatusConflict || body["code"] != "snapshot_in_use" {
		t.Fatalf("delete v1 in use: status %d (%v), want 409 snapshot_in_use", resp.StatusCode, body)
	}

	// Release the lease: retention re-runs and v1 goes (S5).
	resp, _ = doReq(t, "DELETE", ts.URL+"/api/leases/"+user, "token-a", nil)
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		t.Fatalf("release: status %d", resp.StatusCode)
	}
	if _, err := db.GetNamedSnapshot(context.Background(), "consumer-a", "warm", 1); err == nil {
		t.Fatal("v1 survived the release of its only live lease")
	}
	// v4 stays (latest).
	if _, err := db.GetNamedSnapshot(context.Background(), "consumer-a", "warm", 4); err != nil {
		t.Fatalf("v4 missing after release: %v", err)
	}
}

// TestSnapshotStartCreateRefusalCodes: the create errors carry the A2
// codes not_found, image_mismatch and cannot_start.
func TestSnapshotStartCreateRefusalCodes(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	svc.cfg.TemplateStoragePath = t.TempDir()
	seedImage(t, db, "py-base", 2048)
	seedImage(t, db, "go-base", 4096)
	src := createLiveLease(t, ts.URL, "token-a", nil)
	saveSnapshotOK(t, ts.URL, "token-a", src, map[string]any{"name": "warm"})

	cases := []struct {
		name       string
		body       map[string]any
		createFn   func() error
		wantStatus int
		wantCode   string
	}{
		{name: "unknown", body: map[string]any{"snapshot": "nope"}, wantStatus: http.StatusNotFound, wantCode: "not_found"},
		{name: "mismatch", body: map[string]any{"snapshot": "warm", "image": "go-base"}, wantStatus: http.StatusBadRequest, wantCode: "image_mismatch"},
		{name: "cannot_start", body: map[string]any{"snapshot": "warm"}, createFn: func() error { return substrate.ErrNotFound }, wantStatus: http.StatusConflict, wantCode: "cannot_start"},
	}
	for _, c := range cases {
		if c.createFn != nil {
			sub.createFn = func(ctx context.Context, req substrate.CreateRequest) (substrate.Sandbox, error) {
				return substrate.Sandbox{}, fmt.Errorf("e2b: create: %w", c.createFn())
			}
		} else {
			sub.createFn = nil
		}
		resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", c.body)
		sub.createFn = nil
		if resp.StatusCode != c.wantStatus || body["code"] != c.wantCode {
			t.Fatalf("%s: status %d (%v), want %d %s", c.name, resp.StatusCode, body, c.wantStatus, c.wantCode)
		}
	}
}
