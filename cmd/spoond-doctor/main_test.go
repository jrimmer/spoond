package spoonddoctor

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jrimmer/spoond/store"
)

// insertBuild records one build row with the given versions and state.
func insertBuild(t *testing.T, db *store.DB, id, state, fc, kernel string) {
	t.Helper()
	now := time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)
	err := db.InsertBuild(context.Background(), store.BuildRow{
		BuildID: id, Kind: "template", TemplateID: "tpl0123456789abcdefgh",
		Image: "py-base", State: state,
		FirecrackerVersion: fc, KernelVersion: kernel,
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("insert build %s: %v", id, err)
	}
}

// TestVersionsInUse lists the distinct versions of non-deleted builds:
// deleted builds and empty version fields are excluded, values are
// deduplicated and sorted. versionsInUse itself opens the database
// read-only, so it must work against an existing, migrated store.
func TestVersionsInUse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spoond.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	insertBuild(t, db, "b-1", "ready", "v1.14-0.2.0", "vmlinux-6.1.177_5008931")
	insertBuild(t, db, "b-2", "ready", "v1.14-0.2.0", "vmlinux-6.1.177_5008931")
	insertBuild(t, db, "b-3", "building", "v1.15-0.1.0", "vmlinux-6.1.177_5008931")
	insertBuild(t, db, "b-4", "deleted", "v1.10-0.1.0", "vmlinux-6.1.100")
	insertBuild(t, db, "b-5", "failed", "", "")
	db.Close()

	t.Setenv("SPOOND_DB_PATH", path)
	fc, kernel, err := versionsInUse()
	if err != nil {
		t.Fatalf("versionsInUse: %v", err)
	}
	wantFC := []string{"v1.14-0.2.0", "v1.15-0.1.0"}
	wantKernel := []string{"vmlinux-6.1.177_5008931"}
	if len(fc) != len(wantFC) {
		t.Fatalf("fc = %v, want %v", fc, wantFC)
	}
	for i := range wantFC {
		if fc[i] != wantFC[i] {
			t.Errorf("fc[%d] = %q, want %q", i, fc[i], wantFC[i])
		}
	}
	if len(kernel) != len(wantKernel) {
		t.Fatalf("kernel = %v, want %v", kernel, wantKernel)
	}
	for i := range wantKernel {
		if kernel[i] != wantKernel[i] {
			t.Errorf("kernel[%d] = %q, want %q", i, kernel[i], wantKernel[i])
		}
	}
}

// TestCheckLeasesLost: the "leases: lost" check lists every lost lease
// with its owner, image, age and the time the GC stops keeping its
// snapshots, PASSes when there are none, and never FAILs. A lease with
// an empty lost_at (lost before the column existed) has no age and no
// keep-until: the line says the grace period starts at the next GC
// pass, which is the pass that stamps the row.
func TestCheckLeasesLost(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spoond.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	now := time.Now().UTC()
	seed := func(id string, persistent bool, lostAt time.Time) {
		t.Helper()
		if err := db.UpsertLease(context.Background(), store.LeaseRow{
			ID: id, Owner: "user-1", Image: "py-base", State: "lost",
			Persistent: persistent, LostAt: lostAt,
			CreatedAt: now, ExpiresAt: now, LastActive: now,
		}); err != nil {
			t.Fatalf("seed lease %s: %v", id, err)
		}
	}
	// A running lease is not reported; a lost one is, with the age and
	// the keep-until time of its grace period (7 d persistent, 1 d not).
	if err := db.UpsertLease(context.Background(), store.LeaseRow{
		ID: "l-run", Owner: "user-1", Image: "go-base", State: "running",
		CreatedAt: now, ExpiresAt: now, LastActive: now,
	}); err != nil {
		t.Fatalf("seed running lease: %v", err)
	}
	seed("l-per", true, now.Add(-48*time.Hour))
	seed("l-plain", false, now.Add(-2*time.Hour))
	seed("l-nostamp", false, time.Time{})
	db.Close()

	t.Setenv("SPOOND_DB_PATH", path)
	results := checkLeases()
	if len(results) != 1 {
		t.Fatalf("checkLeases = %+v, want one result", results)
	}
	got := results[0]
	if got.name != "leases: lost" {
		t.Errorf("name = %q", got.name)
	}
	if got.status != "WARN" {
		t.Errorf("status = %q, want WARN", got.status)
	}
	for _, want := range []string{
		"3 lease(s) lost",
		"l-per owner=user-1 image=py-base lost 48h0m0s ago",
		"l-plain owner=user-1 image=py-base lost 2h0m0s ago",
		// The unstamped row carries no age and no keep-until: the grace
		// period starts when the GC stamps it.
		"l-nostamp owner=user-1 image=py-base lost before tracking began (grace starts at the next GC pass)",
	} {
		if !strings.Contains(got.detail, want) {
			t.Errorf("detail lacks %q:\n%s", want, got.detail)
		}
	}
	if strings.Contains(got.detail, "l-run") {
		t.Errorf("a running lease was reported:\n%s", got.detail)
	}
	// The keep-until time is lost_at + the grace period: 48 h ago + 7 d
	// and 2 h ago + 1 d. The unstamped loss has none — it is not counted
	// from now, because the GC's stamp, not the doctor's clock, fixes
	// when its grace period started.
	until := map[string]time.Time{
		"l-per":   now.Add(-48 * time.Hour).Add(7 * 24 * time.Hour),
		"l-plain": now.Add(-2 * time.Hour).Add(24 * time.Hour),
	}
	for id, want := range until {
		wantStr := "snapshot kept until " + want.UTC().Format("2006-01-02 15:04 Z07:00")
		if !strings.Contains(got.detail, id+" ") || !strings.Contains(got.detail, wantStr) {
			t.Errorf("detail lacks %s's %q:\n%s", id, wantStr, got.detail)
		}
	}

	// With no lost leases the check PASSes, and it never FAILs: even a
	// missing database is only a WARN.
	path2 := filepath.Join(t.TempDir(), "clean.db")
	db2, err := store.Open(path2)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db2.Close()
	t.Setenv("SPOOND_DB_PATH", path2)
	results = checkLeases()
	if len(results) != 1 || results[0].status != "PASS" || results[0].detail != "none" {
		t.Fatalf("clean checkLeases = %+v, want PASS none", results)
	}

	t.Setenv("SPOOND_DB_PATH", filepath.Join(t.TempDir(), "missing.db"))
	results = checkLeases()
	if len(results) != 1 || results[0].status != "WARN" {
		t.Fatalf("missing-db checkLeases = %+v, want WARN", results)
	}
}
