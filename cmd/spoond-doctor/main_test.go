package spoonddoctor

import (
	"context"
	"path/filepath"
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
