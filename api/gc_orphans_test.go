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
// directory is needed and never reaped, even in quarantine mode and with
// an old mtime.
func TestReapOrphansKeepsCatalogBuild(t *testing.T) {
	svc, buf, db, _ := gcTestService(t)
	t.Setenv("ORPHAN_REAP", "quarantine")
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
	t.Setenv("ORPHAN_REAP", "quarantine")
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

	// The reap quarantines the orphan; removal takes a second pass after
	// the quarantine period, which the delete-after-age test covers.
	if _, _ = svc.reapOrphans(context.Background()); dirExists(dirOrphan) {
		t.Errorf("unreferenced orphan %s stayed in the storage path", orphan)
	}
	for _, d := range []string{dirA, dirB, dirC} {
		if !dirExists(d) {
			t.Errorf("header-referenced build %s was removed", filepath.Base(d))
		}
	}
	// The orphan is now in quarantine, not deleted.
	if !dirExists(filepath.Join(svc.quarantineDir(), orphan)) {
		t.Errorf("orphan %s was not quarantined", orphan)
	}
}

// TestReapOrphansQuarantinesDeletedAndUnknown: a catalog build marked
// deleted and a directory the catalog never recorded are both orphans and
// are moved to quarantine (not deleted) in quarantine mode.
func TestReapOrphansQuarantinesDeletedAndUnknown(t *testing.T) {
	svc, buf, db, _ := gcTestService(t)
	t.Setenv("ORPHAN_REAP", "quarantine")
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
	if reaped != 0 {
		t.Errorf("reaped = %d, want 0 (quarantine only)", reaped)
	}
	if freed != 0 {
		t.Errorf("freed = %d, want 0 (quarantine only)", freed)
	}
	if dirExists(dirDeleted) || dirExists(dirUnknown) {
		t.Errorf("a deleted or unknown directory stayed in the storage path; log:\n%s", buf.String())
	}
	for _, id := range []string{deleted, unknown} {
		if !dirExists(filepath.Join(svc.quarantineDir(), id)) {
			t.Errorf("orphan %s was not quarantined", id)
		}
	}
}

// TestReapOrphansQuarantineRestore: a quarantined directory that a later
// pass needs again is moved back into the storage path and its marker
// dropped.
func TestReapOrphansQuarantineRestore(t *testing.T) {
	svc, _, db, _ := gcTestService(t)
	t.Setenv("ORPHAN_REAP", "quarantine")
	// Keep the root set non-empty with an unrelated live build.
	seedGCBuild(t, db, e2b.NewUUID(), "template", "", "consumer-a", "ready", e2b.NewTemplateID())
	id := e2b.NewUUID()
	dir := mkOrphanDir(t, svc.cfg.TemplateStoragePath, id)
	ageDir(t, dir)

	if _, _ = svc.reapOrphans(context.Background()); dirExists(dir) {
		t.Fatalf("orphan was not quarantined")
	}
	qdir := filepath.Join(svc.quarantineDir(), id)
	if !dirExists(qdir) {
		t.Fatalf("orphan not found in quarantine")
	}
	// A catalog build row naming the quarantined id makes it needed.
	seedGCBuild(t, db, id, "pause", "", "consumer-a", "ready", e2b.NewTemplateID())
	svc.reapOrphans(context.Background())

	if !dirExists(dir) {
		t.Errorf("needed quarantined directory was not restored")
	}
	if dirExists(qdir) {
		t.Errorf("quarantined copy survived the restore")
	}
	if _, err := os.Stat(filepath.Join(dir, orphanQuarantineMarker)); !os.IsNotExist(err) {
		t.Errorf("marker survived the restore: %v", err)
	}
}

// TestReapOrphansQuarantineDeletesAfterAge: a quarantined directory is
// deleted only after ORPHAN_QUARANTINE_SECS, and the marker date survives
// a restart (a fresh Service reading the same directory).
func TestReapOrphansQuarantineDeletesAfterAge(t *testing.T) {
	svc, buf, db, _ := gcTestService(t)
	t.Setenv("ORPHAN_REAP", "quarantine")
	seedGCBuild(t, db, e2b.NewUUID(), "template", "", "consumer-a", "ready", e2b.NewTemplateID())
	id := e2b.NewUUID()
	dir := mkOrphanDir(t, svc.cfg.TemplateStoragePath, id)
	ageDir(t, dir)

	if _, _ = svc.reapOrphans(context.Background()); dirExists(dir) {
		t.Fatalf("orphan was not quarantined")
	}
	qdir := filepath.Join(svc.quarantineDir(), id)
	if !dirExists(qdir) {
		t.Fatalf("quarantined directory missing")
	}

	// A fresh service (a restart) sees a young quarantine and deletes
	// nothing.
	svc2, _, db2, _ := gcTestService(t)
	svc2.cfg.TemplateStoragePath = svc.cfg.TemplateStoragePath
	seedGCBuild(t, db2, e2b.NewUUID(), "template", "", "consumer-a", "ready", e2b.NewTemplateID())
	if reaped, _ := svc2.reapOrphans(context.Background()); reaped != 0 {
		t.Errorf("young quarantine reaped = %d, want 0", reaped)
	}
	if !dirExists(qdir) {
		t.Fatalf("young quarantined directory was removed")
	}

	// With no quarantine period left, the next pass deletes it.
	t.Setenv("ORPHAN_QUARANTINE_SECS", "1")
	time.Sleep(1100 * time.Millisecond)
	reaped, freed := svc2.reapOrphans(context.Background())
	if reaped != 1 {
		t.Errorf("aged quarantine reaped = %d, want 1; log:\n%s", reaped, buf.String())
	}
	if freed <= 0 {
		t.Errorf("freed = %d, want > 0", freed)
	}
	if dirExists(qdir) {
		t.Errorf("aged quarantined directory survived")
	}
	if n := counterValue(t, svc2.metrics.GCOrphansReaped); n != 1 {
		t.Errorf("gc_orphans_reaped_total = %v, want 1", n)
	}
	if n := counterValue(t, svc2.metrics.GCOrphanBytesReaped); n <= 0 {
		t.Errorf("gc_orphan_bytes_reaped_total = %v, want > 0", n)
	}
}

// TestReapOrphansQuarantineMarkerSurvivesRestart: the marker, not the
// directory mtime, dates the quarantine, so a marker written in the past
// is honoured by a service that just started.
func TestReapOrphansQuarantineMarkerSurvivesRestart(t *testing.T) {
	svc, _, db, _ := gcTestService(t)
	t.Setenv("ORPHAN_REAP", "quarantine")
	seedGCBuild(t, db, e2b.NewUUID(), "template", "", "consumer-a", "ready", e2b.NewTemplateID())
	id := e2b.NewUUID()
	dir := mkOrphanDir(t, svc.cfg.TemplateStoragePath, id)
	ageDir(t, dir)
	if _, _ = svc.reapOrphans(context.Background()); dirExists(dir) {
		t.Fatalf("orphan was not quarantined")
	}
	qdir := filepath.Join(svc.quarantineDir(), id)
	// Backdate the marker (and the directory) by two days: past the
	// default 24 h quarantine.
	old := time.Now().Add(-48 * time.Hour)
	if err := writeQuarantineMarker(qdir, old); err != nil {
		t.Fatalf("backdate marker: %v", err)
	}

	svc2, _, db2, _ := gcTestService(t)
	svc2.cfg.TemplateStoragePath = svc.cfg.TemplateStoragePath
	seedGCBuild(t, db2, e2b.NewUUID(), "template", "", "consumer-a", "ready", e2b.NewTemplateID())
	if reaped, _ := svc2.reapOrphans(context.Background()); reaped != 1 {
		t.Errorf("reaped = %d, want 1 (marker dated it)", reaped)
	}
	if dirExists(qdir) {
		t.Errorf("quarantined directory survived despite an old marker")
	}
}

// TestReapOrphansOff: ORPHAN_REAP=off leaves everything alone, even a
// clear orphan.
func TestReapOrphansOff(t *testing.T) {
	svc, buf, db, _ := gcTestService(t)
	t.Setenv("ORPHAN_REAP", "off")
	seedGCBuild(t, db, e2b.NewUUID(), "template", "", "consumer-a", "ready", e2b.NewTemplateID())
	orphan := e2b.NewUUID()
	dir := mkOrphanDir(t, svc.cfg.TemplateStoragePath, orphan)
	ageDir(t, dir)

	if reaped, _ := svc.reapOrphans(context.Background()); reaped != 0 {
		t.Errorf("reaped = %d, want 0 with ORPHAN_REAP=off", reaped)
	}
	if !dirExists(dir) {
		t.Errorf("ORPHAN_REAP=off touched the orphan")
	}
	if strings.Contains(buf.String(), orphan) {
		t.Errorf("ORPHAN_REAP=off logged the orphan:\n%s", buf.String())
	}
}

// TestReapOrphansKeepsRecent: a directory modified within
// ORPHAN_MIN_AGE_SECS may still be in use, so it is never touched.
func TestReapOrphansKeepsRecent(t *testing.T) {
	svc, _, db, _ := gcTestService(t)
	t.Setenv("ORPHAN_REAP", "quarantine")
	seedGCBuild(t, db, e2b.NewUUID(), "template", "", "consumer-a", "ready", e2b.NewTemplateID())
	recent := e2b.NewUUID()
	dir := mkOrphanDir(t, svc.cfg.TemplateStoragePath, recent) // mtime now

	if reaped, _ := svc.reapOrphans(context.Background()); reaped != 0 {
		t.Errorf("reaped = %d, want 0: a recent directory was touched", reaped)
	}
	if !dirExists(dir) {
		t.Errorf("recent directory was quarantined")
	}
}

// TestReapOrphansDryRun: the default (ORPHAN_REAP unset) logs the orphan
// and changes nothing.
func TestReapOrphansDryRun(t *testing.T) {
	svc, buf, db, _ := gcTestService(t)
	t.Setenv("ORPHAN_REAP", "")
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
	if dirExists(filepath.Join(svc.quarantineDir(), orphan)) {
		t.Errorf("dry run quarantined the orphan")
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
	t.Setenv("ORPHAN_REAP", "quarantine")
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
	t.Setenv("ORPHAN_REAP", "quarantine")
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
	t.Setenv("ORPHAN_REAP", "quarantine")

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
