package api

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jrimmer/spoond/v2/store"
	"github.com/jrimmer/spoond/v2/substrate/e2b"
)

// mkOrphanDir creates a build-like directory under root with one file, so
// the reap has a size to measure and a non-zero mtime.
func mkOrphanDir(t *testing.T, root, name string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "memfile"), make([]byte, 4096), 0o644); err != nil {
		t.Fatalf("write %s: %v", dir, err)
	}
	return dir
}

// ageDir pushes dir's mtime two hours back, past the orphan reap's
// one-hour minimum age, so the age rule does not spare it.
func ageDir(t *testing.T, dir string) {
	t.Helper()
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatalf("chtimes %s: %v", dir, err)
	}
}

// dirExists reports whether a directory (not a symlink) is there.
func dirExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// TestReapOrphansKeepsCatalogBuild: a non-deleted catalog build's
// directory is needed and never reaped, even with GC_DELETE=1 and an old
// mtime.
func TestReapOrphansKeepsCatalogBuild(t *testing.T) {
	svc, buf, db, _ := gcTestService(t)
	t.Setenv("GC_DELETE", "1")
	buildID := e2b.NewUUID()
	seedGCBuild(t, db, buildID, "template", "", "consumer-a", "ready", e2b.NewTemplateID())
	dir := mkOrphanDir(t, svc.cfg.TemplateStoragePath, buildID)
	ageDir(t, dir)

	reaped, _ := svc.reapOrphans(context.Background())
	if reaped != 0 {
		t.Errorf("reaped = %d, want 0", reaped)
	}
	if !dirExists(dir) {
		t.Errorf("needed catalog build directory was removed")
	}
	if strings.Contains(buf.String(), buildID) {
		t.Errorf("needed build logged by the reap:\n%s", buf.String())
	}
}

// TestReapOrphansKeepsHeaderChain: a needed build's header names B as raw
// UUID bytes and B's header names C; B and C are kept transitively, while
// an old, unrecorded, unreferenced directory is reaped.
func TestReapOrphansKeepsHeaderChain(t *testing.T) {
	svc, _, db, _ := gcTestService(t)
	t.Setenv("GC_DELETE", "1")
	a := e2b.NewUUID()
	seedGCBuild(t, db, a, "template", "", "consumer-a", "ready", e2b.NewTemplateID())
	b, c, orphan := e2b.NewUUID(), e2b.NewUUID(), e2b.NewUUID()
	dirA := mkOrphanDir(t, svc.cfg.TemplateStoragePath, a)
	dirB := mkOrphanDir(t, svc.cfg.TemplateStoragePath, b)
	dirC := mkOrphanDir(t, svc.cfg.TemplateStoragePath, c)
	dirOrphan := mkOrphanDir(t, svc.cfg.TemplateStoragePath, orphan)

	// A's memfile header records B's raw 16-byte UUID; B's rootfs header
	// records C's. The textual form is exercised too.
	bu, err := uuid.Parse(b)
	if err != nil {
		t.Fatalf("parse %s: %v", b, err)
	}
	if err := os.WriteFile(filepath.Join(dirA, "memfile.header"), bu[:], 0o644); err != nil {
		t.Fatalf("write header: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dirB, "rootfs.ext4.header"), []byte(c), 0o644); err != nil {
		t.Fatalf("write header: %v", err)
	}
	for _, d := range []string{dirA, dirB, dirC, dirOrphan} {
		ageDir(t, d)
	}

	if reaped, _ := svc.reapOrphans(context.Background()); reaped != 1 {
		t.Errorf("reaped = %d, want 1 (the unreferenced orphan)", reaped)
	}
	for _, d := range []string{dirA, dirB, dirC} {
		if !dirExists(d) {
			t.Errorf("header-referenced build %s was removed", filepath.Base(d))
		}
	}
	if dirExists(dirOrphan) {
		t.Errorf("unreferenced orphan %s survived", orphan)
	}
}

// TestReapOrphansReapsDeletedAndUnknown: a catalog build marked deleted
// and a directory the catalog never recorded are both orphans and go.
func TestReapOrphansReapsDeletedAndUnknown(t *testing.T) {
	svc, buf, db, _ := gcTestService(t)
	t.Setenv("GC_DELETE", "1")
	// One non-deleted build keeps the root set non-empty.
	seedGCBuild(t, db, e2b.NewUUID(), "template", "", "consumer-a", "ready", e2b.NewTemplateID())
	deleted := e2b.NewUUID()
	seedGCBuild(t, db, deleted, "pause", "", "consumer-a", "deleted", e2b.NewTemplateID())
	unknown := e2b.NewUUID()
	dirDeleted := mkOrphanDir(t, svc.cfg.TemplateStoragePath, deleted)
	dirUnknown := mkOrphanDir(t, svc.cfg.TemplateStoragePath, unknown)
	ageDir(t, dirDeleted)
	ageDir(t, dirUnknown)

	reaped, freed := svc.reapOrphans(context.Background())
	if reaped != 2 {
		t.Errorf("reaped = %d, want 2", reaped)
	}
	if freed <= 0 {
		t.Errorf("freed = %d, want > 0", freed)
	}
	if dirExists(dirDeleted) || dirExists(dirUnknown) {
		t.Errorf("a deleted or unknown directory survived; log:\n%s", buf.String())
	}
	if n := counterValue(t, svc.metrics.GCOrphansReaped); n != 2 {
		t.Errorf("gc_orphans_reaped_total = %v, want 2", n)
	}
	if n := counterValue(t, svc.metrics.GCOrphanBytesReaped); n <= 0 {
		t.Errorf("gc_orphan_bytes_reaped_total = %v, want > 0", n)
	}
}

// TestReapOrphansKeepsRecent: a directory modified within
// ORPHAN_MIN_AGE_SECS may still be in use, so it is never touched.
func TestReapOrphansKeepsRecent(t *testing.T) {
	svc, _, db, _ := gcTestService(t)
	t.Setenv("GC_DELETE", "1")
	seedGCBuild(t, db, e2b.NewUUID(), "template", "", "consumer-a", "ready", e2b.NewTemplateID())
	recent := e2b.NewUUID()
	dir := mkOrphanDir(t, svc.cfg.TemplateStoragePath, recent) // mtime now

	if reaped, _ := svc.reapOrphans(context.Background()); reaped != 0 {
		t.Errorf("reaped = %d, want 0: a recent directory was touched", reaped)
	}
	if !dirExists(dir) {
		t.Errorf("recent directory was removed")
	}
}

// TestReapOrphansDryRun: the default (GC_DELETE unset) logs the orphan
// and removes nothing.
func TestReapOrphansDryRun(t *testing.T) {
	svc, buf, db, _ := gcTestService(t)
	t.Setenv("GC_DELETE", "")
	seedGCBuild(t, db, e2b.NewUUID(), "template", "", "consumer-a", "ready", e2b.NewTemplateID())
	orphan := e2b.NewUUID()
	dir := mkOrphanDir(t, svc.cfg.TemplateStoragePath, orphan)
	ageDir(t, dir)

	reaped, _ := svc.reapOrphans(context.Background())
	if reaped != 0 {
		t.Errorf("reaped = %d, want 0 in dry run", reaped)
	}
	if !dirExists(dir) {
		t.Errorf("dry run removed the orphan")
	}
	if !strings.Contains(buf.String(), "gc: would reap orphan "+orphan) {
		t.Errorf("dry run did not log the orphan:\n%s", buf.String())
	}
}

// TestReapOrphansIgnoresNonUUIDAndSymlink: only direct child directories
// whose name parses as a UUID may be touched; a plain name and a symlink
// (pointing outside the storage path) are left alone.
func TestReapOrphansIgnoresNonUUIDAndSymlink(t *testing.T) {
	svc, _, db, _ := gcTestService(t)
	t.Setenv("GC_DELETE", "1")
	seedGCBuild(t, db, e2b.NewUUID(), "template", "", "consumer-a", "ready", e2b.NewTemplateID())

	plain := mkOrphanDir(t, svc.cfg.TemplateStoragePath, "not-a-uuid")
	ageDir(t, plain)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "keep"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	link := filepath.Join(svc.cfg.TemplateStoragePath, e2b.NewUUID())
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if reaped, _ := svc.reapOrphans(context.Background()); reaped != 0 {
		t.Errorf("reaped = %d, want 0", reaped)
	}
	if !dirExists(plain) {
		t.Errorf("non-UUID directory was removed")
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("symlink was touched: %v", err)
	}
	if !dirExists(outside) {
		t.Errorf("symlink target was followed and removed")
	}
}

// TestReapOrphansEmptyCatalogSkips: with no needed builds in the catalog
// the root set is untrustworthy, so the reap refuses and leaves
// everything, logging the skip rather than failing.
func TestReapOrphansEmptyCatalogSkips(t *testing.T) {
	svc, buf, _, _ := gcTestService(t)
	t.Setenv("GC_DELETE", "1")
	orphan := e2b.NewUUID()
	dir := mkOrphanDir(t, svc.cfg.TemplateStoragePath, orphan)
	ageDir(t, dir)

	reaped, _ := svc.reapOrphans(context.Background())
	if reaped != 0 {
		t.Errorf("reaped = %d, want 0 with an empty catalog", reaped)
	}
	if !dirExists(dir) {
		t.Errorf("empty-catalog pass removed a directory")
	}
	if !strings.Contains(buf.String(), "orphan reap skipped") {
		t.Errorf("empty-catalog pass did not log the skip:\n%s", buf.String())
	}
}

// TestReapOrphansLiveReferences: a directory named only by a lease, a
// kept build, a sandbox or an image's current build is needed and kept.
func TestReapOrphansLiveReferences(t *testing.T) {
	svc, _, db, _ := gcTestService(t)
	t.Setenv("GC_DELETE", "1")

	resume, ckpt, kept, sbBuild, current := e2b.NewUUID(), e2b.NewUUID(), e2b.NewUUID(), e2b.NewUUID(), e2b.NewUUID()
	// One non-deleted build keeps the root set non-empty (the roots
	// below name no builds rows, which is the point).
	seedGCBuild(t, db, e2b.NewUUID(), "template", "", "consumer-a", "ready", e2b.NewTemplateID())
	if err := db.UpsertLease(context.Background(), store.LeaseRow{
		ID: "l1", Owner: "consumer-a", Image: "py-base",
		ResumeBuildID: resume, LastCheckpointBuildID: ckpt,
		CreatedAt: gcOld, ExpiresAt: gcOld, LastActive: gcOld, State: "running",
		Class: "guaranteed",
	}); err != nil {
		t.Fatalf("seed lease: %v", err)
	}
	if err := db.KeepBuild(context.Background(), "l1", kept, time.Now()); err != nil {
		t.Fatalf("keep build: %v", err)
	}
	if err := db.UpsertSandbox(context.Background(), store.SandboxRow{
		SandboxID: "s1", LeaseID: "l1", BuildID: sbBuild, ExecutionID: "exec-s1",
		VCPU: 2, MemoryMB: 2048, StartedAt: gcOld, EndAt: gcOld,
	}); err != nil {
		t.Fatalf("seed sandbox: %v", err)
	}
	seedGCImage(t, db, "py-base", current)

	for _, id := range []string{resume, ckpt, kept, sbBuild, current} {
		dir := mkOrphanDir(t, svc.cfg.TemplateStoragePath, id)
		ageDir(t, dir)
	}
	if reaped, _ := svc.reapOrphans(context.Background()); reaped != 0 {
		t.Errorf("reaped = %d, want 0: a live-referenced directory was removed", reaped)
	}
}
