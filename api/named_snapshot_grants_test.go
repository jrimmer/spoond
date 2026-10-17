package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/jrimmer/spoond/v3/store"
	"github.com/jrimmer/spoond/v3/substrate"
)

// Named-snapshot grants (#141). The fixtures go through the real routes:
// leases are created, saved, granted and started over the API, so every
// row is the shape production writes. consumer-a (token-a) owns the
// snapshots and consumer-b (token-b) is the grantee.

// newGrantServer is newTestServerWithService ready for snapshot saves
// and starts.
func newGrantServer(t *testing.T) (string, *Service, *store.DB, *testSub) {
	t.Helper()
	ts, svc, db, sub := newTestServerWithService(t)
	svc.cfg.TemplateStoragePath = t.TempDir()
	roomyDisk(svc) // the creates, saves and starts must fit in the snapshot disk (FS3b)
	return ts.URL, svc, db, sub
}

// grantSnapshot posts a grant over the API.
func grantSnapshot(t *testing.T, ts, token, name, grantee string) (*http.Response, map[string]any) {
	t.Helper()
	return doReq(t, "POST", ts+"/api/named-snapshot-grants", token, map[string]any{"name": name, "grantee": grantee})
}

// grantSnapshotOK posts a grant and asserts 201.
func grantSnapshotOK(t *testing.T, ts, token, name, grantee string) map[string]any {
	t.Helper()
	resp, body := grantSnapshot(t, ts, token, name, grantee)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("grant %s to %s: status %d: %v", name, grantee, resp.StatusCode, body)
	}
	return body
}

// storedHadSecrets reads a lease's persisted had_secrets.
func storedHadSecrets(t *testing.T, db *store.DB, id string) bool {
	t.Helper()
	row, err := db.GetLease(context.Background(), id)
	if err != nil {
		t.Fatalf("stored lease %s: %v", id, err)
	}
	return row.HadSecrets
}

// savedSecretFree saves lease id as name and returns the version's
// secret_free as the save response reports it and as the row stores it.
func savedSecretFree(t *testing.T, ts string, db *store.DB, id, name string) bool {
	t.Helper()
	resp, body := saveSnapshot(t, ts, "token-a", id, map[string]any{"name": name})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("save %s: status %d: %v", name, resp.StatusCode, body)
	}
	row, err := db.GetNamedSnapshot(context.Background(), "consumer-a", name, int64(body["version"].(float64)))
	if err != nil {
		t.Fatalf("saved row %s: %v", name, err)
	}
	if got, _ := body["secret_free"].(bool); got != row.SecretFree {
		t.Fatalf("save response secret_free = %v, row = %v", got, row.SecretFree)
	}
	return row.SecretFree
}

// TestSnapshotGrantStartCrossOwner: the owner grants a secret-free name
// to another user, who starts a lease from it with snapshot_owner. The
// lease is the grantee's (charged to it), its snapshot object names the
// owner, the copy-side markers name the new lease, and both sides list
// the grant. A repeat grant answers the first row.
func TestSnapshotGrantStartCrossOwner(t *testing.T) {
	ts, svc, db, sub := newGrantServer(t)
	src := createLiveLease(t, ts, "token-a", nil)
	saveSnapshotOK(t, ts, "token-a", src, map[string]any{"name": "honey/warm"})

	first := grantSnapshotOK(t, ts, "token-a", "honey/warm", "consumer-b")
	if first["owner"] != "consumer-a" || first["name"] != "honey/warm" || first["grantee"] != "consumer-b" || first["created_at"] == "" {
		t.Fatalf("grant body = %v", first)
	}
	if again := grantSnapshotOK(t, ts, "token-a", "honey/warm", "consumer-b"); again["created_at"] != first["created_at"] {
		t.Fatalf("repeat grant = %v, want the first row %v", again, first)
	}

	out := createFromSnapshot(t, ts, "token-b", map[string]any{"snapshot": "honey/warm", "snapshot_owner": "consumer-a", "ttl": 300})
	id := out["id"].(string)
	view, _ := out["snapshot"].(map[string]any)
	if view["name"] != "honey/warm" || view["version"].(float64) != 1 || view["owner"] != "consumer-a" {
		t.Fatalf("snapshot object = %v, want honey/warm@1 owned by consumer-a", view)
	}
	l := svc.store.leases[id]
	if l == nil || l.Owner != "consumer-b" {
		t.Fatalf("lease = %+v, want owner consumer-b", l)
	}
	if l.HadSecrets || storedHadSecrets(t, db, id) {
		t.Fatal("a start from a secret-free version must not have had secrets")
	}
	if l.MemoryMB != 2048 {
		t.Fatalf("grantee's lease charged %d MiB, want the version's 2048", l.MemoryMB)
	}
	sb, err := db.GetSandboxByLease(context.Background(), id)
	if err != nil {
		t.Fatalf("sandbox: %v", err)
	}
	if data, err := sub.Fake.ReadFile(t.Context(), sb.SandboxID, leaseIDPath, 1024); err != nil || strings.TrimSpace(string(data)) != id {
		t.Fatalf("lease-id = %q (%v), want %s", data, err, id)
	}
	if marker := readStartedFrom(t, sub, sb.SandboxID); marker["name"] != "honey/warm" || marker["build_id"] != l.SnapshotBuildID {
		t.Fatalf("started-from = %v", marker)
	}

	// The detail, read back from the store's build id, names the owner too.
	l.SnapshotName, l.SnapshotVersion, l.SnapshotOwner = "", 0, ""
	resp, detail := doReq(t, "GET", ts+"/api/leases/"+id, "token-b", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("detail: %d %v", resp.StatusCode, detail)
	}
	if dv, _ := detail["snapshot"].(map[string]any); dv["owner"] != "consumer-a" || dv["name"] != "honey/warm" {
		t.Fatalf("detail snapshot = %v", detail["snapshot"])
	}

	resp, list := doReq(t, "GET", ts+"/api/named-snapshot-grants", "token-a", nil)
	if resp.StatusCode != http.StatusOK || len(list["given"].([]any)) != 1 || len(list["received"].([]any)) != 0 {
		t.Fatalf("owner's grants = %d %v", resp.StatusCode, list)
	}
	resp, list = doReq(t, "GET", ts+"/api/named-snapshot-grants", "token-b", nil)
	rec, _ := list["received"].([]any)
	if resp.StatusCode != http.StatusOK || len(rec) != 1 || rec[0].(map[string]any)["owner"] != "consumer-a" || len(list["given"].([]any)) != 0 {
		t.Fatalf("grantee's grants = %d %v", resp.StatusCode, list)
	}

	// snapshot_owner naming the caller itself is an ordinary own start:
	// consumer-b has no such name.
	resp, body := doReq(t, "POST", ts+"/api/leases", "token-b", map[string]any{"snapshot": "honey/warm", "snapshot_owner": "consumer-b"})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("own-name start = %d %v, want 404", resp.StatusCode, body)
	}
}

// TestSnapshotGrantNotFoundIndistinguishable: without a grant, another
// owner's real name answers exactly what an unknown name, an unknown
// owner or a granted name's missing version answers, so a caller learns
// nothing about names it was not granted.
func TestSnapshotGrantNotFoundIndistinguishable(t *testing.T) {
	ts, _, _, _ := newGrantServer(t)
	src := createLiveLease(t, ts, "token-a", nil)
	saveSnapshotOK(t, ts, "token-a", src, map[string]any{"name": "warm"})
	saveSnapshotOK(t, ts, "token-a", src, map[string]any{"name": "granted"})
	grantSnapshotOK(t, ts, "token-a", "granted", "consumer-b")
	// A tainted name answers 404 too, never 409 not_secret_free, to a
	// caller without a grant.
	dirty := createLiveLease(t, ts, "token-a", map[string]any{"image": "py-base", "ttl": 300, "secrets": map[string]string{"A": "1"}})
	saveSnapshotOK(t, ts, "token-a", dirty, map[string]any{"name": "dirty"})

	start := func(snapshot, owner string) (int, string) {
		resp, body := doReq(t, "POST", ts+"/api/leases", "token-b", map[string]any{"snapshot": snapshot, "snapshot_owner": owner})
		raw, _ := json.Marshal(body)
		return resp.StatusCode, string(raw)
	}
	wantStatus, want := start("nosuch", "consumer-a")
	if wantStatus != http.StatusNotFound || !strings.Contains(want, `"not_found"`) {
		t.Fatalf("unknown name = %d %s, want 404 not_found", wantStatus, want)
	}
	for _, c := range []struct{ snapshot, owner string }{
		{"warm", "consumer-a"},      // real name, no grant
		{"dirty", "consumer-a"},     // real tainted name, no grant
		{"warm", "legacy-consumer"}, // real owner without the name
		{"warm", "nobody-at-all"},   // unknown owner
		{"nosuch", "nobody"},        // both unknown
	} {
		if st, got := start(c.snapshot, c.owner); st != wantStatus || got != want {
			t.Fatalf("%s owned by %s = %d %s, want %d %s", c.snapshot, c.owner, st, got, wantStatus, want)
		}
	}
	// The same with a pinned version (the message names the reference).
	wantStatus, want = start("warm@1", "legacy-consumer")
	if wantStatus != http.StatusNotFound {
		t.Fatalf("warm@1 of an owner without it = %d %s, want 404", wantStatus, want)
	}
	for _, c := range []struct{ snapshot, owner string }{
		{"warm@1", "consumer-a"}, // real version, no grant
		{"warm@1", "nobody"},     // unknown owner
	} {
		if st, got := start(c.snapshot, c.owner); st != wantStatus || got != want {
			t.Fatalf("%s owned by %s = %d %s, want %d %s", c.snapshot, c.owner, st, got, wantStatus, want)
		}
	}
	// A granted name's missing version is the plain not found for that
	// reference, the same shape.
	if st, got := start("granted@9", "consumer-a"); st != http.StatusNotFound || !strings.Contains(got, `"not_found"`) {
		t.Fatalf("granted@9 = %d %s, want 404 not_found", st, got)
	}
}

// TestSnapshotGrantTaintedVersion: a version saved from a lease that had
// a secret is not secret-free. Granting a name whose latest version is
// tainted answers 409 not_secret_free, and a grantee's start of a
// tainted version answers 409 not_secret_free while a clean version of
// the same granted name still starts.
func TestSnapshotGrantTaintedVersion(t *testing.T) {
	ts, _, db, _ := newGrantServer(t)
	clean := createLiveLease(t, ts, "token-a", nil)
	tainted := createLiveLease(t, ts, "token-a", map[string]any{"image": "py-base", "ttl": 300, "secrets": map[string]string{"TOKEN": "s"}})

	if !savedSecretFree(t, ts, db, clean, "warm") {
		t.Fatal("a version saved from a lease that never had a secret must be secret-free")
	}
	if savedSecretFree(t, ts, db, tainted, "dirty") {
		t.Fatal("a version saved from a lease with create-time secrets must not be secret-free")
	}
	resp, body := grantSnapshot(t, ts, "token-a", "dirty", "consumer-b")
	if resp.StatusCode != http.StatusConflict || body["code"] != "not_secret_free" || !strings.Contains(body["error"].(string), "re-save") {
		t.Fatalf("grant of tainted name = %d %v, want 409 not_secret_free", resp.StatusCode, body)
	}

	grantSnapshotOK(t, ts, "token-a", "warm", "consumer-b")
	// warm@2 comes from the tainted lease: the latest is now tainted.
	if savedSecretFree(t, ts, db, tainted, "warm") {
		t.Fatal("warm@2 must not be secret-free")
	}
	resp, body = doReq(t, "POST", ts+"/api/leases", "token-b", map[string]any{"snapshot": "warm", "snapshot_owner": "consumer-a"})
	if resp.StatusCode != http.StatusConflict || body["code"] != "not_secret_free" {
		t.Fatalf("start of tainted latest = %d %v, want 409 not_secret_free", resp.StatusCode, body)
	}
	createFromSnapshot(t, ts, "token-b", map[string]any{"snapshot": "warm@1", "snapshot_owner": "consumer-a"})
	// The owner itself still starts its tainted version: the rule guards
	// only what crosses owners.
	createFromSnapshot(t, ts, "token-a", map[string]any{"snapshot": "warm"})

	// The list shows secret_free per version.
	resp, list := doReq(t, "GET", ts+"/api/named-snapshots?prefix=warm", "token-a", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: %d %v", resp.StatusCode, list)
	}
	for _, n := range list["snapshots"].([]any) {
		for _, v := range n.(map[string]any)["versions"].([]any) {
			vv := v.(map[string]any)
			if want := vv["version"].(float64) == 1; vv["secret_free"] != want {
				t.Fatalf("warm@%v secret_free = %v, want %v", vv["version"], vv["secret_free"], want)
			}
		}
	}
}

// TestSnapshotGrantRevoke: revoking stops new starts (404, as if never
// granted) but leaves a lease already started through the grant running;
// revoking again is 404.
func TestSnapshotGrantRevoke(t *testing.T) {
	ts, svc, _, _ := newGrantServer(t)
	src := createLiveLease(t, ts, "token-a", nil)
	saveSnapshotOK(t, ts, "token-a", src, map[string]any{"name": "warm"})
	grantSnapshotOK(t, ts, "token-a", "warm", "consumer-b")
	running := createFromSnapshot(t, ts, "token-b", map[string]any{"snapshot": "warm", "snapshot_owner": "consumer-a"})["id"].(string)

	// Only the owner can revoke its grant.
	resp, _ := doReq(t, "DELETE", ts+"/api/named-snapshot-grants?name=warm&grantee=consumer-b", "token-b", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("grantee revoke = %d, want 404", resp.StatusCode)
	}
	resp, _ = doReq(t, "DELETE", ts+"/api/named-snapshot-grants?name=warm&grantee=consumer-b", "token-a", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke = %d, want 204", resp.StatusCode)
	}
	resp, body := doReq(t, "POST", ts+"/api/leases", "token-b", map[string]any{"snapshot": "warm", "snapshot_owner": "consumer-a"})
	if resp.StatusCode != http.StatusNotFound || body["code"] != "not_found" {
		t.Fatalf("start after revoke = %d %v, want 404 not_found", resp.StatusCode, body)
	}
	if l := svc.store.leases[running]; l == nil || !l.live() {
		t.Fatalf("the running lease must survive the revoke: %+v", l)
	}
	resp, detail := doReq(t, "GET", ts+"/api/leases/"+running, "token-b", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("running lease detail after revoke = %d %v", resp.StatusCode, detail)
	}
	resp, _ = doReq(t, "DELETE", ts+"/api/named-snapshot-grants?name=warm&grantee=consumer-b", "token-a", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("second revoke = %d, want 404", resp.StatusCode)
	}
	// Revoking also ends restores to the start snapshot.
	row, err := svc.db.GetNamedSnapshotLatest(context.Background(), "consumer-a", "warm")
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	resp, _ = doReq(t, "POST", ts+"/api/leases/"+running+"/restore", "token-b", map[string]any{"build_id": row.BuildID})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("restore to start after revoke = %d, want 404", resp.StatusCode)
	}
}

// TestSnapshotGrantGoesWithTheName: deleting a name's last version drops
// its grants, so a later save under the same name is private until
// granted again.
func TestSnapshotGrantGoesWithTheName(t *testing.T) {
	ts, _, db, _ := newGrantServer(t)
	src := createLiveLease(t, ts, "token-a", nil)
	saveSnapshotOK(t, ts, "token-a", src, map[string]any{"name": "warm"})
	grantSnapshotOK(t, ts, "token-a", "warm", "consumer-b")
	resp, body := doReq(t, "DELETE", ts+"/api/named-snapshots/warm", "token-a", nil)
	if resp.StatusCode/100 != 2 {
		t.Fatalf("delete name = %d %v", resp.StatusCode, body)
	}
	if _, err := db.GetNamedSnapshotGrant(context.Background(), "consumer-a", "warm", "consumer-b"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("grant after the name was deleted = %v, want gone", err)
	}
	saveSnapshotOK(t, ts, "token-a", src, map[string]any{"name": "warm"})
	resp, body = doReq(t, "POST", ts+"/api/leases", "token-b", map[string]any{"snapshot": "warm", "snapshot_owner": "consumer-a"})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("start of the re-saved name without a new grant = %d %v, want 404", resp.StatusCode, body)
	}
}

// TestSnapshotGrantValidation: the grant route's refusals.
func TestSnapshotGrantValidation(t *testing.T) {
	ts, _, _, _ := newGrantServer(t)
	src := createLiveLease(t, ts, "token-a", nil)
	saveSnapshotOK(t, ts, "token-a", src, map[string]any{"name": "warm"})
	for _, c := range []struct {
		name, grantee string
		status        int
		code          string
	}{
		{"warm", "consumer-a", http.StatusBadRequest, "bad_request"},   // self
		{"warm", "no-such-user", http.StatusBadRequest, "bad_request"}, // unknown grantee
		{"warm", "", http.StatusBadRequest, "bad_request"},
		{"Bad Name", "consumer-b", http.StatusBadRequest, "bad_request"},
		{"cold", "consumer-b", http.StatusNotFound, "not_found"}, // caller has no such name
	} {
		resp, body := grantSnapshot(t, ts, "token-a", c.name, c.grantee)
		if resp.StatusCode != c.status || body["code"] != c.code {
			t.Fatalf("grant %q to %q = %d %v, want %d %s", c.name, c.grantee, resp.StatusCode, body, c.status, c.code)
		}
	}
	// Another owner cannot grant consumer-a's name.
	if resp, body := grantSnapshot(t, ts, "token-b", "warm", "legacy-consumer"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("grant of another owner's name = %d %v, want 404", resp.StatusCode, body)
	}
	if resp, body := doReq(t, "POST", ts+"/api/leases", "token-b", map[string]any{"image": "py-base", "snapshot_owner": "consumer-a"}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("snapshot_owner without snapshot = %d %v, want 400", resp.StatusCode, body)
	}
}

// TestHadSecretsSetBySecretPaths: create-time secrets, an exec with
// secrets and a background job with secrets each mark the lease (in
// memory and in its row) and make its saves not secret-free; an exec
// with only env values does not.
func TestHadSecretsSetBySecretPaths(t *testing.T) {
	ts, svc, db, _ := newGrantServer(t)

	created := createLiveLease(t, ts, "token-a", map[string]any{"image": "py-base", "ttl": 300, "secrets": map[string]string{"A": "1"}})
	if !svc.store.leases[created].HadSecrets || !storedHadSecrets(t, db, created) {
		t.Fatal("create-time secrets did not mark the lease")
	}
	// The mark survives a backend restart: the lease loads from its row.
	if row, err := db.GetLease(context.Background(), created); err != nil || !rowToLease(row).HadSecrets {
		t.Fatalf("a lease loaded from its row lost had_secrets (%v)", err)
	}

	execd := createLiveLease(t, ts, "token-a", nil)
	resp, body := doReq(t, "POST", ts+"/api/leases/"+execd+"/exec", "token-a",
		map[string]any{"cmd": "true", "secrets": map[string]string{"B": "2"}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("exec with secrets: %d %v", resp.StatusCode, body)
	}
	if !svc.store.leases[execd].HadSecrets || !storedHadSecrets(t, db, execd) {
		t.Fatal("exec-time secrets did not mark the lease")
	}
	if savedSecretFree(t, ts, db, execd, "after-exec") {
		t.Fatal("a save after an exec with secrets must not be secret-free")
	}

	job := createLiveLease(t, ts, "token-a", nil)
	resp, body = doReq(t, "POST", ts+"/api/leases/"+job+"/exec", "token-a",
		map[string]any{"cmd": "true", "background": true, "secrets": map[string]string{"C": "3"}})
	if resp.StatusCode/100 != 2 {
		t.Fatalf("job with secrets: %d %v", resp.StatusCode, body)
	}
	if !svc.store.leases[job].HadSecrets || !storedHadSecrets(t, db, job) {
		t.Fatal("job secrets did not mark the lease")
	}

	envOnly := createLiveLease(t, ts, "token-a", nil)
	resp, body = doReq(t, "POST", ts+"/api/leases/"+envOnly+"/exec", "token-a",
		map[string]any{"cmd": "true", "env": map[string]string{"GOFLAGS": "-mod=mod"}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("exec with env: %d %v", resp.StatusCode, body)
	}
	if svc.store.leases[envOnly].HadSecrets || storedHadSecrets(t, db, envOnly) {
		t.Fatal("env values are not secrets and must not mark the lease")
	}
	if !savedSecretFree(t, ts, db, envOnly, "env-only") {
		t.Fatal("a save from a lease that only had env values must be secret-free")
	}
}

// TestHadSecretsInherited: clones and forks carry the source's flag, a
// start from a version that is not secret-free starts marked, and a save
// from such a start is not secret-free either (the taint follows the
// memory). The clean controls stay clean.
func TestHadSecretsInherited(t *testing.T) {
	ts, svc, db, _ := newGrantServer(t)
	dirty := createLiveLease(t, ts, "token-a", map[string]any{"image": "py-base", "ttl": 300, "secrets": map[string]string{"A": "1"}})
	clean := createLiveLease(t, ts, "token-a", nil)

	for _, c := range []struct {
		src  string
		want bool
	}{{dirty, true}, {clean, false}} {
		resp, body := doReq(t, "POST", ts+"/api/leases/"+c.src+"/clone", "token-a", nil)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("clone: %d %v", resp.StatusCode, body)
		}
		cid := body["id"].(string)
		if svc.store.leases[cid].HadSecrets != c.want || storedHadSecrets(t, db, cid) != c.want {
			t.Fatalf("clone of %s had_secrets = %v, want %v", c.src, svc.store.leases[cid].HadSecrets, c.want)
		}
		resp, body = doReq(t, "POST", ts+"/api/leases/"+c.src+"/fork", "token-a", map[string]any{"count": 1})
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("fork: %d %v", resp.StatusCode, body)
		}
		fid := body["ids"].([]any)[0].(string)
		if svc.store.leases[fid].HadSecrets != c.want || storedHadSecrets(t, db, fid) != c.want {
			t.Fatalf("fork of %s had_secrets = %v, want %v", c.src, svc.store.leases[fid].HadSecrets, c.want)
		}
	}

	if savedSecretFree(t, ts, db, dirty, "dirty") || !savedSecretFree(t, ts, db, clean, "clean") {
		t.Fatal("source saves have the wrong secret_free")
	}
	fromDirty := createFromSnapshot(t, ts, "token-a", map[string]any{"snapshot": "dirty"})["id"].(string)
	fromClean := createFromSnapshot(t, ts, "token-a", map[string]any{"snapshot": "clean"})["id"].(string)
	if !svc.store.leases[fromDirty].HadSecrets || !storedHadSecrets(t, db, fromDirty) {
		t.Fatal("a start from a tainted version must start marked")
	}
	if svc.store.leases[fromClean].HadSecrets || storedHadSecrets(t, db, fromClean) {
		t.Fatal("a start from a secret-free version must start clean")
	}
	if savedSecretFree(t, ts, db, fromDirty, "relaundered") {
		t.Fatal("re-saving a start from a tainted version must not launder it")
	}
}

// TestSnapshotGrantRetentionAfterGranteeRelease: a version the grantee
// runs from is spared by the owner's retention, and the grantee's release
// re-runs the owner's retention, which then drops it (S5 across owners).
func TestSnapshotGrantRetentionAfterGranteeRelease(t *testing.T) {
	ts, _, db, _ := newGrantServer(t)
	src := createLiveLease(t, ts, "token-a", nil)
	saveSnapshotOK(t, ts, "token-a", src, map[string]any{"name": "warm", "keep": 2})
	grantSnapshotOK(t, ts, "token-a", "warm", "consumer-b")
	user := createFromSnapshot(t, ts, "token-b", map[string]any{"snapshot": "warm@1", "snapshot_owner": "consumer-a", "persistent": true})["id"].(string)

	for range 3 {
		saveSnapshotOK(t, ts, "token-a", src, map[string]any{"name": "warm"})
	}
	if _, err := db.GetNamedSnapshot(context.Background(), "consumer-a", "warm", 1); err != nil {
		t.Fatalf("v1 pruned while the grantee's lease runs from it: %v", err)
	}
	resp, _ := doReq(t, "DELETE", ts+"/api/leases/"+user, "token-b", nil)
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		t.Fatalf("release: %d", resp.StatusCode)
	}
	if _, err := db.GetNamedSnapshot(context.Background(), "consumer-a", "warm", 1); err == nil {
		t.Fatal("v1 survived the release of the grantee's lease, its only user")
	}
	if _, err := db.GetNamedSnapshot(context.Background(), "consumer-a", "warm", 4); err != nil {
		t.Fatalf("v4 missing: %v", err)
	}
}

// TestRestoreToStartSnapshot: a lease started through a grant restores
// to its start build (another owner's): the guest is a fresh copy of the
// version, its lease-id and started-from markers name this lease, the
// saving lease's secret files are gone and the lease's own create-time
// secret is back, and the generation bumps. Nobody else can restore to
// that build: not a lease of the grantee that started elsewhere, not a
// clone of the started lease, not the owner's own source lease.
func TestRestoreToStartSnapshot(t *testing.T) {
	ts, svc, db, sub := newGrantServer(t)
	src := createLiveLease(t, ts, "token-a", nil)
	saveSnapshotOK(t, ts, "token-a", src, map[string]any{"name": "warm"})
	grantSnapshotOK(t, ts, "token-a", "warm", "consumer-b")
	row, err := db.GetNamedSnapshotLatest(context.Background(), "consumer-a", "warm")
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	// The grantee's create-time secret marks its lease (had_secrets), which
	// does not matter to a restore of its own lease.
	out := createFromSnapshot(t, ts, "token-b", map[string]any{
		"snapshot": "warm", "snapshot_owner": "consumer-a", "persistent": true,
		"secrets": map[string]string{"MINE": "m"},
	})
	id := out["id"].(string)
	l := svc.store.leases[id]
	oldSandbox := l.SandboxID
	// As after a backend restart: the name, version and owner are looked
	// up again from the build id.
	l.SnapshotName, l.SnapshotVersion, l.SnapshotOwner = "", 0, ""

	// Every sandbox created from here on carries a stray secret file, as
	// a version's memory could.
	sub.createFn = func(ctx context.Context, req substrate.CreateRequest) (substrate.Sandbox, error) {
		sb, err := sub.Fake.Create(ctx, req)
		if err != nil {
			return sb, err
		}
		_ = sub.Fake.WriteFile(ctx, sb.ID, "/run/secrets/SOURCE", []byte("s"), 0o600)
		_ = sub.Fake.WriteFile(ctx, sb.ID, leaseIDPath, []byte(src+"\n"), 0o644)
		return sb, nil
	}
	t.Cleanup(func() { sub.createFn = nil })

	resp, body := doReq(t, "POST", ts+"/api/leases/"+id+"/restore", "token-b", map[string]any{"build_id": row.BuildID})
	if resp.StatusCode != http.StatusOK || body["build_id"] != row.BuildID || body["generation"].(float64) != 2 {
		t.Fatalf("restore to start = %d %v, want 200 at generation 2", resp.StatusCode, body)
	}
	if l.SandboxID == oldSandbox {
		t.Fatal("restore must replace the sandbox")
	}
	if data, err := sub.Fake.ReadFile(t.Context(), l.SandboxID, leaseIDPath, 1024); err != nil || strings.TrimSpace(string(data)) != id {
		t.Fatalf("lease-id after restore = %q (%v), want %s", data, err, id)
	}
	if marker := readStartedFrom(t, sub, l.SandboxID); marker["name"] != "warm" || marker["version"].(float64) != 1 || marker["build_id"] != row.BuildID {
		t.Fatalf("started-from after restore = %v", marker)
	}
	if _, err := sub.Fake.ReadFile(t.Context(), l.SandboxID, "/run/secrets/SOURCE", 1024); err == nil {
		t.Fatal("the restored guest still holds a stray secret file")
	}
	if data, err := sub.Fake.ReadFile(t.Context(), l.SandboxID, "/run/secrets/MINE", 1024); err != nil || string(data) != "m" {
		t.Fatalf("own secret after restore = %q (%v), want m", data, err)
	}
	if l.SnapshotOwner != "consumer-a" || l.SnapshotName != "warm" {
		t.Fatalf("snapshot facts after restore = %q %q", l.SnapshotOwner, l.SnapshotName)
	}

	// Nobody else may restore to it.
	other := createLiveLease(t, ts, "token-b", nil)
	resp, body = doReq(t, "POST", ts+"/api/leases/"+id+"/clone", "token-b", nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("clone: %d %v", resp.StatusCode, body)
	}
	clone := body["id"].(string)
	for _, c := range []struct{ lease, token string }{{other, "token-b"}, {clone, "token-b"}} {
		resp, body := doReq(t, "POST", ts+"/api/leases/"+c.lease+"/restore", c.token, map[string]any{"build_id": row.BuildID})
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("restore of %s to another's start build = %d %v, want 404", c.lease, resp.StatusCode, body)
		}
	}
	// The owner's own source lease saved the version but did not start
	// from it, and its last checkpoint has moved on: no restore either.
	if _, err := svc.checkpointLease(t.Context(), svc.store.leases[src]); err != nil {
		t.Fatalf("checkpoint source: %v", err)
	}
	if resp, body := doReq(t, "POST", ts+"/api/leases/"+src+"/restore", "token-a", map[string]any{"build_id": row.BuildID}); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("source restore to the version = %d %v, want 404", resp.StatusCode, body)
	}
}

// TestRestoreToStartMarkerFailureKeepsLease: a restore to the start
// snapshot whose copy-side marker cannot be written fails and leaves the
// lease on its old sandbox, as a snapshot start fails the create (S3).
func TestRestoreToStartMarkerFailureKeepsLease(t *testing.T) {
	ts, svc, db, sub := newGrantServer(t)
	src := createLiveLease(t, ts, "token-a", nil)
	saveSnapshotOK(t, ts, "token-a", src, map[string]any{"name": "warm"})
	row, _ := db.GetNamedSnapshotLatest(context.Background(), "consumer-a", "warm")
	id := createFromSnapshot(t, ts, "token-a", map[string]any{"snapshot": "warm", "persistent": true})["id"].(string)
	l := svc.store.leases[id]
	oldSandbox, gen := l.SandboxID, l.Generation

	// Every WriteFile fails: the copy-side markers cannot land.
	sub.FailCall("WriteFile", 0, fmt.Errorf("disk gone"))
	resp, body := doReq(t, "POST", ts+"/api/leases/"+id+"/restore", "token-a", map[string]any{"build_id": row.BuildID})
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("restore with failing marker = %d %v, want 500", resp.StatusCode, body)
	}
	if l.SandboxID != oldSandbox || l.Generation != gen || !l.live() {
		t.Fatalf("lease changed by a failed restore: sandbox %s gen %d", l.SandboxID, l.Generation)
	}
}

// TestSnapshotGrantIdentityUsersAndUserDelete: with identity users, the
// grantee and snapshot_owner may be given by name (stored and shown as
// the user id). Deleting a user removes the grants it gave and the
// grants it received; a failed grants step answers 500 and a retry
// finishes it.
func TestSnapshotGrantIdentityUsersAndUserDelete(t *testing.T) {
	ts, svc, _, _, ids, victimID := newUserDeleteServer(t)
	svc.cfg.TemplateStoragePath = t.TempDir()
	admin := ids.UserByName("admin")
	if admin == nil {
		t.Fatal("no admin user")
	}

	victimLease := createLiveLease(t, ts.URL, "victim-tok", nil)
	if resp, body := saveSnapshot(t, ts.URL, "victim-tok", victimLease, map[string]any{"name": "warm"}); resp.StatusCode != http.StatusCreated {
		t.Fatalf("victim save: %d %v", resp.StatusCode, body)
	}
	adminLease := createLiveLease(t, ts.URL, "admin-tok", nil)
	if resp, body := saveSnapshot(t, ts.URL, "admin-tok", adminLease, map[string]any{"name": "mine"}); resp.StatusCode != http.StatusCreated {
		t.Fatalf("admin save: %d %v", resp.StatusCode, body)
	}
	given := grantSnapshotOK(t, ts.URL, "victim-tok", "warm", "admin")
	if given["grantee"] != admin.ID || given["owner"] != victimID {
		t.Fatalf("grant by name = %v, want grantee %s owner %s", given, admin.ID, victimID)
	}
	grantSnapshotOK(t, ts.URL, "admin-tok", "mine", victimID)

	out := createFromSnapshot(t, ts.URL, "admin-tok", map[string]any{"snapshot": "warm", "snapshot_owner": "victim"})
	if view := out["snapshot"].(map[string]any); view["owner"] != victimID {
		t.Fatalf("snapshot owner = %v, want %s", view["owner"], victimID)
	}
	if resp, _ := doReq(t, "DELETE", ts.URL+"/api/leases/"+out["id"].(string), "admin-tok", nil); resp.StatusCode/100 != 2 {
		t.Fatalf("release: %d", resp.StatusCode)
	}

	svc.userDeleteStoreErr = func(step string) error {
		if step == userDeleteStepDropGrants {
			return fmt.Errorf("database is locked")
		}
		return nil
	}
	resp, body := doReq(t, "DELETE", ts.URL+"/api/users/"+victimID, "admin-tok", nil)
	if resp.StatusCode != http.StatusInternalServerError || body["step"] != userDeleteStepDropGrants {
		t.Fatalf("delete with a failing grants step = %d %v, want 500 at %s", resp.StatusCode, body, userDeleteStepDropGrants)
	}
	svc.userDeleteStoreErr = nil
	// As after a backend restart: the in-memory deleted mark is gone, so
	// only the leftover grants tell the retry there is state to remove.
	svc.ownerDeleteMu.Lock()
	delete(svc.deletedOwners, victimID)
	svc.ownerDeleteMu.Unlock()
	if resp, body := doReq(t, "DELETE", ts.URL+"/api/users/"+victimID, "admin-tok", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("retry delete = %d %v, want 200", resp.StatusCode, body)
	}

	resp, list := doReq(t, "GET", ts.URL+"/api/named-snapshot-grants", "admin-tok", nil)
	if resp.StatusCode != http.StatusOK || len(list["given"].([]any)) != 0 || len(list["received"].([]any)) != 0 {
		t.Fatalf("admin's grants after the victim's delete = %d %v, want none", resp.StatusCode, list)
	}
}
