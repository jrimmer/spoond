package api

import (
	"context"
	"strings"
	"testing"

	"github.com/jrimmer/spoond/v2/store"
	"github.com/jrimmer/spoond/v2/substrate/e2b"
)

// The GC selection rule under test: a build is never a candidate while
// it is the parent of any non-deleted build, or a ref_build_id in
// build_refs of a kept build. Both protections are structural — they
// follow the builds/build_refs tables alone, not the lease, sandbox and
// image rows that name a build directly.

// seedParentChain writes an orphan template build t0 and its live
// ("ready") checkpoint child p, both older than the GC's age rule, so
// only the parent structure decides their fate. The image points at
// its own separate current build, so neither t0 nor p is an image's
// current_build_id, and no lease or sandbox row names them.
func seedParentChain(t *testing.T, db *store.DB, image string) (t0, p string) {
	t.Helper()
	tid := e2b.NewTemplateID()
	seedGCImage(t, db, image, e2b.NewUUID()) // the image's own current build
	t0, p = e2b.NewUUID(), e2b.NewUUID()
	seedGCBuild(t, db, t0, "template", "", "", "ready", tid)
	seedGCBuild(t, db, p, "checkpoint", t0, "consumer-a", "ready", tid)
	return t0, p
}

// TestGCProtectsParentOfLiveBuild: a template build that is the
// parent_build_id of a non-deleted (ready) build is never a candidate,
// even when no image, lease or sandbox row names it — deleting it would
// delete snapshot layers the live child build depends on. The child
// itself, referenced by nothing, stays a candidate: the protection
// follows references, it is not a blanket keep.
func TestGCProtectsParentOfLiveBuild(t *testing.T) {
	svc, buf, db, _ := gcTestService(t)
	t0, p := seedParentChain(t, db, "py-base")

	if err := svc.gcOnce(context.Background()); err != nil {
		t.Fatalf("gc: %v", err)
	}
	lines := buf.String()
	if strings.Contains(lines, "would delete "+t0) {
		t.Errorf("parent of a live build proposed for delete:\n%s", lines)
	}
	if !strings.Contains(lines, "would delete "+p) {
		t.Errorf("unreferenced child not proposed:\n%s", lines)
	}
}

// TestGCProtectsParentOfLiveBuildWithDelete: with GC_DELETE=1 the
// parent of a live build survives on disk (no substrate delete) and in
// the catalog, while the unreferenced child is deleted.
func TestGCProtectsParentOfLiveBuildWithDelete(t *testing.T) {
	svc, _, db, sub := gcTestService(t)
	t.Setenv("GC_DELETE", "1")
	t0, p := seedParentChain(t, db, "py-base")

	if err := svc.gcOnce(context.Background()); err != nil {
		t.Fatalf("gc: %v", err)
	}
	b, err := db.GetBuild(context.Background(), t0)
	if err != nil {
		t.Fatalf("get build %s: %v", t0, err)
	}
	if b.State == "deleted" {
		t.Errorf("build %s deleted while a non-deleted build names it as parent", t0)
	}
	if n := calls(sub.Fake, "DeleteBuild"); n != 1 {
		t.Errorf("DeleteBuild calls = %d, want 1 (the unreferenced child only)", n)
	}
	if b, _ := db.GetBuild(context.Background(), p); b.State != "deleted" {
		t.Errorf("unreferenced child state = %q, want deleted", b.State)
	}
}

// TestGCProtectsParentOfDeletedBuildNotKept: a deleted child protects
// nothing above it — its files are gone, so its parent is a candidate
// again once nothing else references it. Chains therefore unwind from
// the tail, one GC pass per link.
func TestGCProtectsParentOfDeletedBuildNotKept(t *testing.T) {
	svc, buf, db, _ := gcTestService(t)
	t0, p := seedParentChain(t, db, "py-base")
	if err := db.UpdateBuildState(context.Background(), p, "deleted", "", nil); err != nil {
		t.Fatalf("mark child deleted: %v", err)
	}

	if err := svc.gcOnce(context.Background()); err != nil {
		t.Fatalf("gc: %v", err)
	}
	if !strings.Contains(buf.String(), "would delete "+t0) {
		t.Errorf("parent of a deleted build not proposed:\n%s", buf.String())
	}
}

// TestGCProtectsBuildRefsReference: a build named as a ref_build_id by
// a kept build (here: kept because it is the parent of a live build) is
// never a candidate, and neither are its own ancestors.
func TestGCProtectsBuildRefsReference(t *testing.T) {
	svc, buf, db, _ := gcTestService(t)
	t0, _ := seedParentChain(t, db, "py-base")
	// A second lineage: base → child, with the kept build t0's headers
	// referencing the child.
	tid := e2b.NewTemplateID()
	base, child := e2b.NewUUID(), e2b.NewUUID()
	seedGCBuild(t, db, base, "template", "", "", "ready", tid)
	seedGCBuild(t, db, child, "checkpoint", base, "consumer-a", "ready", tid)
	if err := db.AddBuildRefs(context.Background(), t0, []string{child}); err != nil {
		t.Fatalf("add build refs: %v", err)
	}

	if err := svc.gcOnce(context.Background()); err != nil {
		t.Fatalf("gc: %v", err)
	}
	lines := buf.String()
	for _, id := range []string{base, child} {
		if strings.Contains(lines, "would delete "+id) {
			t.Errorf("build_refs-referenced build %s proposed for delete:\n%s", id, lines)
		}
		b, err := db.GetBuild(context.Background(), id)
		if err != nil {
			t.Fatalf("get build %s: %v", id, err)
		}
		if b.State != "ready" {
			t.Errorf("build %s state = %q, want ready", id, b.State)
		}
	}
}

// TestGCProtectsAncestorChain: the whole ancestor chain of a live
// build is kept. Chain template → c1 → c2 → c3 with only the tail (c3)
// named by a lease row: c2, c1 and the template all survive because
// each is the parent of a non-deleted build.
func TestGCProtectsAncestorChain(t *testing.T) {
	svc, buf, db, _ := gcTestService(t)
	tid := e2b.NewTemplateID()
	root := e2b.NewUUID()
	seedGCImage(t, db, "py-base", root)
	c1, c2, c3 := e2b.NewUUID(), e2b.NewUUID(), e2b.NewUUID()
	seedGCBuild(t, db, root, "template", "", "", "ready", tid)
	seedGCBuild(t, db, c1, "checkpoint", root, "consumer-a", "ready", tid)
	seedGCBuild(t, db, c2, "checkpoint", c1, "consumer-a", "ready", tid)
	seedGCBuild(t, db, c3, "checkpoint", c2, "consumer-a", "ready", tid)
	// Only the newest checkpoint is a root (a lease's
	// last_checkpoint_build_id); there is no sandbox row.
	if err := db.UpsertLease(context.Background(), store.LeaseRow{
		ID: "l1", Owner: "consumer-a", Image: "py-base",
		LastCheckpointBuildID: c3,
		CreatedAt:             gcOld, ExpiresAt: gcOld, LastActive: gcOld, State: "running",
		Class: "guaranteed",
	}); err != nil {
		t.Fatalf("seed lease: %v", err)
	}

	if err := svc.gcOnce(context.Background()); err != nil {
		t.Fatalf("gc: %v", err)
	}
	if lines := buf.String(); strings.Contains(lines, "would delete") {
		t.Errorf("an ancestor of a live build was proposed:\n%s", lines)
	}
	for _, id := range []string{root, c1, c2, c3} {
		b, err := db.GetBuild(context.Background(), id)
		if err != nil {
			t.Fatalf("get build %s: %v", id, err)
		}
		if b.State != "ready" {
			t.Errorf("ancestor %s state = %q, want ready", id, b.State)
		}
	}
}

// TestGCProtectsCurrentBuildID: an image's current_build_id is a root
// whatever state its build row is in, and a current_build_id that names
// no builds row at all is tolerated (kept, and never a candidate).
func TestGCProtectsCurrentBuildID(t *testing.T) {
	svc, buf, db, _ := gcTestService(t)
	tid := e2b.NewTemplateID()
	current := e2b.NewUUID()
	seedGCBuild(t, db, current, "template", "", "", "ready", tid)
	seedGCImage(t, db, "py-base", current)
	// A second image whose current build was never recorded in builds.
	seedGCImage(t, db, "go-base", e2b.NewUUID())

	if err := svc.gcOnce(context.Background()); err != nil {
		t.Fatalf("gc: %v", err)
	}
	if lines := buf.String(); strings.Contains(lines, "would delete "+current) {
		t.Errorf("an image's current build was proposed:\n%s", lines)
	}
	b, err := db.GetBuild(context.Background(), current)
	if err != nil {
		t.Fatalf("get build: %v", err)
	}
	if b.State != "ready" {
		t.Errorf("current build state = %q, want ready", b.State)
	}
}

// TestGCLiveLeaseResumeAndCheckpointProtected: a live lease's
// resume_build_id and last_checkpoint_build_id are roots, and their
// whole ancestor chains are kept with them. A lost lease keeps nothing.
func TestGCLiveLeaseResumeAndCheckpointProtected(t *testing.T) {
	svc, buf, db, _ := gcTestService(t)
	tid := e2b.NewTemplateID()
	root := e2b.NewUUID()
	seedGCImage(t, db, "py-base", root)
	pause := e2b.NewUUID()
	ckpt := e2b.NewUUID()
	seedGCBuild(t, db, root, "template", "", "", "ready", tid)
	seedGCBuild(t, db, pause, "pause", root, "consumer-a", "ready", tid)
	seedGCBuild(t, db, ckpt, "checkpoint", root, "consumer-a", "ready", tid)
	lease := func(id, state string) {
		if err := db.UpsertLease(context.Background(), store.LeaseRow{
			ID: id, Owner: "consumer-a", Image: "py-base",
			ResumeBuildID: pause, LastCheckpointBuildID: ckpt,
			CreatedAt: gcOld, ExpiresAt: gcOld, LastActive: gcOld, State: state,
			Class: "guaranteed",
		}); err != nil {
			t.Fatalf("seed lease %s: %v", id, err)
		}
	}
	lease("l-live", "running")
	lease("l-lost", "lost")

	if err := svc.gcOnce(context.Background()); err != nil {
		t.Fatalf("gc: %v", err)
	}
	lines := buf.String()
	for _, id := range []string{root, pause, ckpt} {
		if strings.Contains(lines, "would delete "+id) {
			t.Errorf("live lease build %s proposed for delete:\n%s", id, lines)
		}
		b, err := db.GetBuild(context.Background(), id)
		if err != nil {
			t.Fatalf("get build %s: %v", id, err)
		}
		if b.State != "ready" {
			t.Errorf("live lease build %s state = %q, want ready", id, b.State)
		}
	}
}

// TestGC20261001DryRunShape is the regression test for the 2026-10-01
// dry-run: four builds were listed as "gc: would delete" while live
// builds still referenced them — py-base checkpoints that are the
// parent_build_id of non-deleted builds, the py-base and dev-base
// template builds in the same position (the dev-base one also a
// ref_build_id in build_refs). None of them may be proposed, and with
// GC_DELETE=1 none may be deleted.
func TestGC20261001DryRunShape(t *testing.T) {
	svc, buf, db, sub := gcTestService(t)
	t.Setenv("GC_DELETE", "1")

	pyTid, devTid := e2b.NewTemplateID(), e2b.NewTemplateID()

	// py-base: the image's current template build, and — from an older
	// build of the same image — a checkpoint chain whose rows are live.
	pyCurrent := e2b.NewUUID()
	seedGCImage(t, db, "py-base", pyCurrent)
	seedGCBuild(t, db, pyCurrent, "template", "", "", "ready", pyTid)

	pyOldTemplate := "3ae34c8d-653c-4298-923d-3b7915229b62" // checkpoint's parent
	ckPy1 := "fbbc35d2-7767-4f26-ab73-1fe25ee6a26c"         // checkpoint, parent of a live build
	ckPy2 := e2b.NewUUID()                                  // the live build on top of it
	seedGCBuild(t, db, pyOldTemplate, "template", "", "", "ready", pyTid)
	seedGCBuild(t, db, ckPy1, "checkpoint", pyOldTemplate, "consumer-a", "ready", pyTid)
	seedGCBuild(t, db, ckPy2, "checkpoint", ckPy1, "consumer-a", "ready", pyTid)
	// The live builds' headers reference the builds their blocks come
	// from (E2B's scheduling metadata); this is what the dry-run missed.
	if err := db.AddBuildRefs(context.Background(), ckPy1, []string{pyOldTemplate}); err != nil {
		t.Fatalf("add build refs: %v", err)
	}
	if err := db.AddBuildRefs(context.Background(), ckPy2, []string{ckPy1, pyOldTemplate}); err != nil {
		t.Fatalf("add build refs: %v", err)
	}

	// dev-base: an old template build superseded by a newer one, but
	// still the parent of a non-deleted checkpoint build that references
	// it in build_refs.
	devCurrent := "b4feadd1-db31-4ba8-853f-8ad058e9b6df"
	seedGCImage(t, db, "dev-base", devCurrent)
	seedGCBuild(t, db, devCurrent, "template", "", "", "ready", devTid)
	devOldTemplate := "c573456b-65da-48d6-9880-481194332637"
	ckDev := e2b.NewUUID()
	seedGCBuild(t, db, devOldTemplate, "template", "", "", "ready", devTid)
	seedGCBuild(t, db, ckDev, "checkpoint", devOldTemplate, "consumer-b", "ready", devTid)
	if err := db.AddBuildRefs(context.Background(), ckDev, []string{devOldTemplate}); err != nil {
		t.Fatalf("add build refs: %v", err)
	}

	if err := svc.gcOnce(context.Background()); err != nil {
		t.Fatalf("gc: %v", err)
	}
	protected := []string{
		pyOldTemplate, // parent of the live ckPy1; ref'd by ckPy1 and ckPy2
		ckPy1,         // parent of the live ckPy2; ref'd by ckPy2
		devOldTemplate, devCurrent, pyCurrent,
	}
	lines := buf.String()
	for _, id := range protected {
		if strings.Contains(lines, id) {
			t.Errorf("build %s proposed for delete while live builds reference it:\n%s", id, lines)
		}
		b, err := db.GetBuild(context.Background(), id)
		if err != nil {
			t.Fatalf("get build %s: %v", id, err)
		}
		if b.State == "deleted" {
			t.Errorf("build %s deleted while live builds reference it", id)
		}
	}
	// The chain tails that nothing references are still reclaimed: the
	// fix must not turn the GC into a no-op.
	for _, id := range []string{ckPy2, ckDev} {
		b, err := db.GetBuild(context.Background(), id)
		if err != nil {
			t.Fatalf("get build %s: %v", id, err)
		}
		if b.State != "deleted" {
			t.Errorf("unreferenced build %s state = %q, want deleted", id, b.State)
		}
	}
	if n := calls(sub.Fake, "DeleteBuild"); n != 2 {
		t.Errorf("DeleteBuild calls = %d, want 2\nlog:\n%s", n, lines)
	}
}

// TestGCPausedSandboxBuildChainProtected: a suspended lease's pause
// build and its whole chain are kept through the lease row alone — the
// sandbox row is gone while paused (pauseLeaseBody drops it), so the
// parent protection is what keeps the underlying template.
func TestGCPausedSandboxBuildChainProtected(t *testing.T) {
	svc, buf, db, _ := gcTestService(t)
	tid := e2b.NewTemplateID()
	root := e2b.NewUUID()
	seedGCImage(t, db, "py-base", root)
	pause := e2b.NewUUID()
	seedGCBuild(t, db, root, "template", "", "", "ready", tid)
	seedGCBuild(t, db, pause, "pause", root, "consumer-a", "ready", tid)
	if err := db.UpsertLease(context.Background(), store.LeaseRow{
		ID: "l1", Owner: "consumer-a", Image: "py-base",
		ResumeBuildID: pause, State: "suspended", Suspended: true, Persistent: true,
		CreatedAt: gcOld, ExpiresAt: gcOld, LastActive: gcOld, Class: "guaranteed",
	}); err != nil {
		t.Fatalf("seed lease: %v", err)
	}

	if err := svc.gcOnce(context.Background()); err != nil {
		t.Fatalf("gc: %v", err)
	}
	if lines := buf.String(); strings.Contains(lines, "would delete") {
		t.Errorf("a suspended lease's chain was proposed:\n%s", lines)
	}
}
