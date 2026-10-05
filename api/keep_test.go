package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v2/store"
	"github.com/jrimmer/spoond/v2/substrate/e2b"
)

// gcWouldDelete runs one dry-run GC pass and reports whether the build
// was proposed for deletion.
func gcWouldDelete(t *testing.T, svc *Service, buf *bytes.Buffer, buildID string) bool {
	t.Helper()
	buf.Reset()
	if err := svc.gcOnce(context.Background()); err != nil {
		t.Fatalf("gc: %v", err)
	}
	return strings.Contains(buf.String(), "would delete "+buildID)
}

// gcDeleted runs one GC pass and reports whether the build's row is
// marked deleted.
func gcDeleted(t *testing.T, svc *Service, db *store.DB, buildID string) bool {
	t.Helper()
	if err := svc.gcOnce(context.Background()); err != nil {
		t.Fatalf("gc: %v", err)
	}
	b, err := db.GetBuild(context.Background(), buildID)
	if err != nil {
		t.Fatalf("get build %s: %v", buildID, err)
	}
	return b.State == "deleted"
}

// TestCheckpointKeepSurvivesGC: a kept checkpoint is not a GC candidate
// while the lease lives, and becomes reclaimable once the lease is
// released (its kept rows gone, the next pass proposes the build). The
// build is seeded old, so only the kept set decides its fate.
func TestCheckpointKeepSurvivesGC(t *testing.T) {
	svc, buf, db, _ := gcTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	l, err := svc.grant(ctx, "c", "py-base", time.Hour, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	p := e2bNewKeptBuild(t, db, l)
	if err := svc.keepBuild(ctx, l.ID, p); err != nil {
		t.Fatalf("keep: %v", err)
	}

	if gcWouldDelete(t, svc, buf, p) {
		t.Fatal("kept build proposed for delete while the lease lives")
	}

	// Releasing the lease drops its kept rows: the next pass proposes the
	// build again.
	svc.release(ctx, l)
	if rows, err := db.ListKeptBuilds(ctx); err != nil || len(rows[l.ID]) != 0 {
		t.Fatalf("kept rows after release = %v (%v), want none", rows, err)
	}
	if !gcWouldDelete(t, svc, buf, p) {
		t.Fatal("released lease's kept build still protected")
	}
}

// TestCheckpointKeepGCDelete: the kept build survives a deleting pass
// while the lease lives and is actually deleted after release.
func TestCheckpointKeepGCDelete(t *testing.T) {
	svc, _, db, sub := gcTestService(t)
	t.Setenv("GC_DELETE", "1")
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	l, err := svc.grant(ctx, "c", "py-base", time.Hour, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	p := e2bNewKeptBuild(t, db, l)
	if err := svc.keepBuild(ctx, l.ID, p); err != nil {
		t.Fatalf("keep: %v", err)
	}
	if gcDeleted(t, svc, db, p) {
		t.Fatal("kept build deleted while the lease lives")
	}
	svc.release(ctx, l)
	if !gcDeleted(t, svc, db, p) {
		t.Fatal("kept build not deleted after the lease's release")
	}
	if n := calls(sub.Fake, "DeleteBuild"); n != 1 {
		t.Fatalf("DeleteBuild calls = %d, want 1", n)
	}
}

// e2bNewKeptBuild seeds an old, ready checkpoint build owned by the
// lease's owner (parent: the lease's live build) and returns its id: a
// build only the kept set can save.
func e2bNewKeptBuild(t *testing.T, db *store.DB, l *Lease) string {
	t.Helper()
	id := newID()
	seedSnapshotBuild(t, db, id, "checkpoint", l.BuildID, l.Owner, "ready")
	return id
}

// TestCheckpointKeepRoute: the checkpoint route accepts {"keep":true},
// records the kept-builds row, reports "kept":true, and the build joins
// the GC kept set; an unkept checkpoint records no row.
func TestCheckpointKeepRoute(t *testing.T) {
	ts, svc, db, _ := newTestServerWithService(t)
	svc.cfg.TemplateStoragePath = t.TempDir()
	seedImage(t, db, "py-base", 2048)

	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a",
		map[string]any{"image": "py-base", "ttl": 300, "persistent": true})
	id := create["id"].(string)

	resp, body := doReq(t, "POST", ts.URL+"/api/leases/"+id+"/checkpoint", "token-a",
		map[string]any{"keep": true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("checkpoint keep = %d (%v), want 200", resp.StatusCode, body)
	}
	buildID, _ := body["build_id"].(string)
	if buildID == "" {
		t.Fatalf("checkpoint build_id empty: %v", body)
	}
	if body["kept"] != true {
		t.Fatalf("checkpoint kept = %v, want true", body["kept"])
	}
	keeps, err := db.ListKeptBuilds(context.Background())
	if err != nil {
		t.Fatalf("list kept: %v", err)
	}
	if len(keeps[id]) != 1 || keeps[id][0] != buildID {
		t.Fatalf("kept rows = %v, want [%s]", keeps, buildID)
	}
	kept, err := svc.keptBuilds(context.Background())
	if err != nil {
		t.Fatalf("keptBuilds: %v", err)
	}
	if !kept[buildID] {
		t.Fatal("kept build not in the GC kept set")
	}

	// A malformed body is 400 — an empty body means "keep nothing" —
	// and pins nothing.
	resp, body = doReq(t, "POST", ts.URL+"/api/leases/"+id+"/checkpoint", "token-a",
		map[string]any{"keep": "banana"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("checkpoint with a garbage body = %d (%v), want 400", resp.StatusCode, body)
	}
	keeps, err = db.ListKeptBuilds(context.Background())
	if err != nil {
		t.Fatalf("list kept: %v", err)
	}
	if len(keeps[id]) != 1 {
		t.Fatalf("kept rows after a garbage body = %v, want still one", keeps)
	}

	// A plain checkpoint records no pin.
	resp, body2 := doReq(t, "POST", ts.URL+"/api/leases/"+id+"/checkpoint", "token-a", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("plain checkpoint = %d (%v), want 200", resp.StatusCode, body2)
	}
	keeps, err = db.ListKeptBuilds(context.Background())
	if err != nil {
		t.Fatalf("list kept: %v", err)
	}
	if len(keeps[id]) != 1 {
		t.Fatalf("kept rows after a plain checkpoint = %v, want still one", keeps)
	}
}

// TestSnapshotDeleteUnpinsKept: DELETE /api/snapshots/{build_id} also
// unpins a kept build (2.3, #121): the pin is not a fence, the owner's
// delete removes the kept row and the build in one call — ahead of the
// lease's own release. The seeded lease's sandbox runs the template
// build, so the kept build is rooted by its keep row alone: while it
// stands, a GC pass cannot touch the build; the route's unpin is what
// lets the delete through.
func TestSnapshotDeleteUnpinsKept(t *testing.T) {
	svc, buf, db, _ := gcTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()
	img, err := db.GetImage(ctx, "py-base")
	if err != nil {
		t.Fatalf("get image: %v", err)
	}
	srv := NewServer(svc, NewImageRegistry(db))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	p := e2b.NewUUID()
	seedSnapshotBuild(t, db, p, "checkpoint", img.CurrentBuildID, "consumer-a", "ready")
	// The pin references a real lease row (the FK cascades on it); the
	// sandbox runs the template build, so nothing else roots p.
	seedGCLease(t, db, "lease-x", "sb-x", img.CurrentBuildID)
	if err := svc.keepBuild(ctx, "lease-x", p); err != nil {
		t.Fatalf("keep: %v", err)
	}
	kept, err := svc.keptBuilds(ctx)
	if err != nil {
		t.Fatalf("keptBuilds: %v", err)
	}
	if !kept[p] {
		t.Fatal("precondition: kept build not in the kept set")
	}
	if gcWouldDelete(t, svc, buf, p) {
		t.Fatal("kept build proposed for delete while its keep row stands (and nothing else roots it)")
	}

	// The delete unpins and removes the build in one call, while the
	// lease that kept it is still alive.
	resp, body := doReq(t, "DELETE", ts.URL+"/api/snapshots/"+p, "token-a", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete kept = %d (%v), want 204 (the delete unpins)", resp.StatusCode, body)
	}
	keeps, err := db.ListKeptBuilds(ctx)
	if err != nil {
		t.Fatalf("list kept: %v", err)
	}
	if len(keeps["lease-x"]) != 0 {
		t.Fatalf("kept rows after the delete = %v, want unpinned", keeps)
	}
	deleted, err := db.GetBuild(ctx, p)
	if err != nil {
		t.Fatalf("get build: %v", err)
	}
	if deleted.State != "deleted" {
		t.Fatalf("build state after the delete = %q, want deleted", deleted.State)
	}
}
