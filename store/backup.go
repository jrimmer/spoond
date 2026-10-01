package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Backup writes a consistent copy of the database into dir with
// VACUUM INTO on the writer, as <prefix>-<YYYYMMDD-HHMMSS>.db (the
// prefix is the database file's basename without extension), then
// deletes all but the newest keep files matching <prefix>-*.db.
func (db *DB) Backup(ctx context.Context, dir string, keep int) error {
	prefix := strings.TrimSuffix(filepath.Base(db.path), filepath.Ext(db.path))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("store: backup dir %s: %w", dir, err)
	}
	target := filepath.Join(dir, prefix+"-"+time.Now().Format("20060102-150405")+".db")
	if _, err := db.w.ExecContext(ctx, `VACUUM INTO '`+strings.ReplaceAll(target, "'", "''")+`'`); err != nil {
		return fmt.Errorf("store: vacuum into %s: %w", target, err)
	}
	matches, err := filepath.Glob(filepath.Join(dir, prefix+"-*.db"))
	if err != nil {
		return fmt.Errorf("store: backup prune %s: %w", dir, err)
	}
	// The timestamp is in the name, so lexicographic order is
	// chronological.
	sort.Strings(matches)
	if excess := len(matches) - keep; excess > 0 {
		for i := range excess {
			if err := os.Remove(matches[i]); err != nil {
				return fmt.Errorf("store: backup prune %s: %w", matches[i], err)
			}
		}
	}
	return nil
}
