package store

import (
	"context"
	"fmt"
	"time"
)

// AddPool records a pooled sandbox for image. created_at is now; the
// pool lists are ordered by it.
func (db *DB) AddPool(ctx context.Context, sandboxID, image string) error {
	_, err := db.w.ExecContext(ctx,
		`INSERT INTO pool (sandbox_id, image, created_at) VALUES (?, ?, ?)`,
		sandboxID, image, formatTime(time.Now()))
	if err != nil {
		return fmt.Errorf("store: add pool %s: %w", sandboxID, err)
	}
	return nil
}

// RemovePool drops a pooled sandbox entry. Idempotent: removing an
// absent entry is not an error.
func (db *DB) RemovePool(ctx context.Context, sandboxID string) error {
	_, err := db.w.ExecContext(ctx, `DELETE FROM pool WHERE sandbox_id = ?`, sandboxID)
	if err != nil {
		return fmt.Errorf("store: remove pool %s: %w", sandboxID, err)
	}
	return nil
}

// ListPool returns the pooled sandbox ids per image, each list ordered
// by created_at.
func (db *DB) ListPool(ctx context.Context) (map[string][]string, error) {
	rows, err := db.r.QueryContext(ctx, `SELECT sandbox_id, image FROM pool ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("store: list pool: %w", err)
	}
	defer rows.Close()
	out := make(map[string][]string)
	for rows.Next() {
		var id, image string
		if err := rows.Scan(&id, &image); err != nil {
			return nil, fmt.Errorf("store: list pool: %w", err)
		}
		out[image] = append(out[image], id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list pool: %w", err)
	}
	return out, nil
}
