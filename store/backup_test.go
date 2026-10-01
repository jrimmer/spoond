package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func TestBackupRoundTripAndPrune(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "spoond.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	backups := filepath.Join(dir, "backups")
	if err := os.MkdirAll(backups, 0o700); err != nil {
		t.Fatalf("mkdir backups: %v", err)
	}
	// Older files the prune must drop, matching the prefix glob.
	for _, name := range []string{"spoond-20200101-000000.db", "spoond-20200102-000000.db"} {
		if err := os.WriteFile(filepath.Join(backups, name), []byte("junk"), 0o600); err != nil {
			t.Fatalf("seed old backup: %v", err)
		}
	}
	// A file with a different prefix stays untouched.
	if err := os.WriteFile(filepath.Join(backups, "other-20200101-000000.db"), []byte("junk"), 0o600); err != nil {
		t.Fatalf("seed other backup: %v", err)
	}

	if err := db.Backup(ctx, backups, 2); err != nil {
		t.Fatalf("backup: %v", err)
	}

	matches, err := filepath.Glob(filepath.Join(backups, "spoond-*.db"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) != 2 {
		t.Fatalf("got %d spoond-*.db files, want 2 (newest backup + 1 kept): %v", len(matches), matches)
	}
	if _, err := os.Stat(filepath.Join(backups, "other-20200101-000000.db")); err != nil {
		t.Fatalf("other-prefix file was removed: %v", err)
	}

	// The fresh backup is a valid database with the migrated schema.
	// Sorted by name, it is the last spoond-*.db file.
	d, err := sql.Open("sqlite", "file:"+matches[len(matches)-1]+"?mode=ro")
	if err != nil {
		t.Fatalf("open backup: %v", err)
	}
	defer d.Close()
	var name string
	if err := d.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'build_refs'`).Scan(&name); err != nil {
		t.Fatalf("backup lacks build_refs table: %v", err)
	}
}

func TestBackupPrefixIsDBBasename(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "staging.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	backups := t.TempDir()
	if err := db.Backup(context.Background(), backups, 7); err != nil {
		t.Fatalf("backup: %v", err)
	}
	matches, _ := filepath.Glob(filepath.Join(backups, "staging-*.db"))
	if len(matches) != 1 {
		t.Fatalf("got %d staging-*.db files, want 1", len(matches))
	}
}
