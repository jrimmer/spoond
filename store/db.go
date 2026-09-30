// Package store implements spoond's embedded SQLite persistence (U05):
// leases, shares, the warm pool and the image/build/sandbox catalog.
// One writer connection serializes every write; a small reader pool
// serves concurrent reads under WAL.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // register the "sqlite" database/sql driver
)

// DB is spoond's embedded SQLite database. One writer connection, a small
// reader pool.
type DB struct {
	w *sql.DB // SetMaxOpenConns(1): all writes serialize here
	r *sql.DB // SetMaxOpenConns(8): reads
}

// ErrNotFound is returned by single-row getters.
var ErrNotFound = errors.New("store: not found")

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Open creates the parent directory (0700) if missing, opens the file
// with the WAL/busy-timeout/foreign-keys/NORMAL DSN on two handles, and
// applies pending migrations in order.
func Open(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("store: create directory: %w", err)
	}
	d := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	w, err := sql.Open("sqlite", d)
	if err != nil {
		return nil, fmt.Errorf("store: open writer: %w", err)
	}
	w.SetMaxOpenConns(1)
	r, err := sql.Open("sqlite", d)
	if err != nil {
		w.Close()
		return nil, fmt.Errorf("store: open reader: %w", err)
	}
	r.SetMaxOpenConns(8)
	db := &DB{w: w, r: r}
	if err := db.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// Close closes both handles.
func (db *DB) Close() error {
	err := db.w.Close()
	if err2 := db.r.Close(); err == nil {
		err = err2
	}
	return err
}

// migration is one embedded .sql file. The file name prefix before the
// first underscore is the version (0001_init.sql → 1).
type migration struct {
	version int
	name    string
	sql     string
}

func loadMigrations() ([]migration, error) {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return nil, fmt.Errorf("store: read migrations: %w", err)
	}
	var out []migration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		prefix, _, _ := strings.Cut(e.Name(), "_")
		v, err := strconv.Atoi(prefix)
		if err != nil {
			return nil, fmt.Errorf("store: migration %q: bad version prefix", e.Name())
		}
		body, err := migrationsFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("store: read migration %q: %w", e.Name(), err)
		}
		out = append(out, migration{version: v, name: e.Name(), sql: string(body)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

// migrate applies pending migrations in order on the writer. Each file
// runs in one transaction and inserts its schema_migrations row. Only
// versions greater than MAX(version) are applied. 0001 contains the
// CREATE TABLE schema_migrations statement itself, so it is applied only
// when that table does not exist yet.
func (db *DB) migrate() error {
	migs, err := loadMigrations()
	if err != nil {
		return err
	}
	ctx := context.Background()

	var table string
	err = db.w.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name='schema_migrations'`).Scan(&table)
	tableExists := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("store: check schema_migrations: %w", err)
	}

	var current int
	if tableExists {
		if err := db.w.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
			return fmt.Errorf("store: read schema version: %w", err)
		}
	}

	for _, m := range migs {
		if tableExists && m.version <= current {
			continue
		}
		if !tableExists && m.version != 1 {
			return fmt.Errorf("store: migration %d pending but schema_migrations table missing", m.version)
		}
		if err := db.applyMigration(ctx, m); err != nil {
			return err
		}
		tableExists = true
		current = m.version
	}
	return nil
}

// applyMigration runs one migration file plus its schema_migrations row
// in a single transaction on the writer.
func (db *DB) applyMigration(ctx context.Context, m migration) error {
	tx, err := db.w.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin %s: %w", m.name, err)
	}
	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		tx.Rollback()
		return fmt.Errorf("store: apply %s: %w", m.name, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
		m.version, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		tx.Rollback()
		return fmt.Errorf("store: record %s: %w", m.name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit %s: %w", m.name, err)
	}
	return nil
}

// formatTime encodes t as RFC 3339 with nanoseconds in UTC; the zero
// time becomes the empty string, the "unset" marker.
func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// parseTime decodes a formatted timestamp; the empty string parses back
// as the zero time.
func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
