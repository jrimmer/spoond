package store

import (
	"context"
	"fmt"
	"time"
)

// ShareRow is one row of the shares table.
type ShareRow struct {
	LeaseID, Grantee, Mode string
	ExpiresAt, CreatedAt   time.Time // ExpiresAt zero = never
}

// UpsertShare inserts the share, or replaces it when the lease/grantee
// pair already exists.
func (db *DB) UpsertShare(ctx context.Context, s ShareRow) error {
	_, err := db.w.ExecContext(ctx, `
INSERT INTO shares (lease_id, grantee, mode, expires_at, created_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(lease_id, grantee) DO UPDATE SET
  mode=excluded.mode,
  expires_at=excluded.expires_at,
  created_at=excluded.created_at`,
		s.LeaseID, s.Grantee, s.Mode, formatTime(s.ExpiresAt), formatTime(s.CreatedAt))
	if err != nil {
		return fmt.Errorf("store: upsert share %s/%s: %w", s.LeaseID, s.Grantee, err)
	}
	return nil
}

// DeleteShare removes one share. Idempotent: deleting an absent share
// is not an error.
func (db *DB) DeleteShare(ctx context.Context, leaseID, grantee string) error {
	_, err := db.w.ExecContext(ctx,
		`DELETE FROM shares WHERE lease_id = ? AND grantee = ?`, leaseID, grantee)
	if err != nil {
		return fmt.Errorf("store: delete share %s/%s: %w", leaseID, grantee, err)
	}
	return nil
}

// ListShares returns every share, ordered by lease and grantee.
func (db *DB) ListShares(ctx context.Context) ([]ShareRow, error) {
	rows, err := db.r.QueryContext(ctx,
		`SELECT lease_id, grantee, mode, expires_at, created_at FROM shares ORDER BY lease_id, grantee`)
	if err != nil {
		return nil, fmt.Errorf("store: list shares: %w", err)
	}
	defer rows.Close()
	var out []ShareRow
	for rows.Next() {
		var s ShareRow
		var expiresAt, createdAt string
		if err := rows.Scan(&s.LeaseID, &s.Grantee, &s.Mode, &expiresAt, &createdAt); err != nil {
			return nil, fmt.Errorf("store: list shares: %w", err)
		}
		s.ExpiresAt = parseTime(expiresAt)
		s.CreatedAt = parseTime(createdAt)
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list shares: %w", err)
	}
	return out, nil
}
