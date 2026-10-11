package store

import (
	"context"
	"fmt"
	"time"
)

// Named-snapshot grants (#141): an owner lets another user start leases
// from one of its names. A grant covers every version of the name; the
// service starts only secret-free versions through it.

// NamedSnapshotGrantRow is one row of the named_snapshot_grants table.
type NamedSnapshotGrantRow struct {
	Owner, Name, Grantee string
	CreatedAt            time.Time
}

// InsertNamedSnapshotGrant records a grant. Idempotent: a repeat keeps
// the first row (and its created_at) and returns it.
func (db *DB) InsertNamedSnapshotGrant(ctx context.Context, g NamedSnapshotGrantRow) (NamedSnapshotGrantRow, error) {
	if _, err := db.w.ExecContext(ctx, `
INSERT INTO named_snapshot_grants (owner, name, grantee, created_at)
VALUES (?, ?, ?, ?)
ON CONFLICT(owner, name, grantee) DO NOTHING`,
		g.Owner, g.Name, g.Grantee, formatTime(g.CreatedAt)); err != nil {
		return NamedSnapshotGrantRow{}, fmt.Errorf("store: insert named snapshot grant %s/%s to %s: %w", g.Owner, g.Name, g.Grantee, err)
	}
	return db.GetNamedSnapshotGrant(ctx, g.Owner, g.Name, g.Grantee)
}

// GetNamedSnapshotGrant returns one grant, or ErrNotFound.
func (db *DB) GetNamedSnapshotGrant(ctx context.Context, owner, name, grantee string) (NamedSnapshotGrantRow, error) {
	rows, err := db.queryNamedSnapshotGrants(ctx,
		`WHERE owner = ? AND name = ? AND grantee = ?`, owner, name, grantee)
	if err != nil {
		return NamedSnapshotGrantRow{}, err
	}
	if len(rows) == 0 {
		return NamedSnapshotGrantRow{}, ErrNotFound
	}
	return rows[0], nil
}

// DeleteNamedSnapshotGrant removes one grant, or returns ErrNotFound when
// it does not exist.
func (db *DB) DeleteNamedSnapshotGrant(ctx context.Context, owner, name, grantee string) error {
	res, err := db.w.ExecContext(ctx,
		`DELETE FROM named_snapshot_grants WHERE owner = ? AND name = ? AND grantee = ?`, owner, name, grantee)
	if err != nil {
		return fmt.Errorf("store: delete named snapshot grant %s/%s to %s: %w", owner, name, grantee, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: delete named snapshot grant %s/%s to %s: %w", owner, name, grantee, err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// NamedSnapshotGrantsGiven lists the grants owner has given, ordered by
// name and grantee.
func (db *DB) NamedSnapshotGrantsGiven(ctx context.Context, owner string) ([]NamedSnapshotGrantRow, error) {
	return db.queryNamedSnapshotGrants(ctx, `WHERE owner = ? ORDER BY name, grantee`, owner)
}

// NamedSnapshotGrantsReceived lists the grants given to grantee, ordered
// by owner and name.
func (db *DB) NamedSnapshotGrantsReceived(ctx context.Context, grantee string) ([]NamedSnapshotGrantRow, error) {
	return db.queryNamedSnapshotGrants(ctx, `WHERE grantee = ? ORDER BY owner, name`, grantee)
}

// DeleteNamedSnapshotGrantsOfUser removes every grant the user gave or
// received. Deleting a user calls it.
func (db *DB) DeleteNamedSnapshotGrantsOfUser(ctx context.Context, user string) error {
	if _, err := db.w.ExecContext(ctx,
		`DELETE FROM named_snapshot_grants WHERE owner = ? OR grantee = ?`, user, user); err != nil {
		return fmt.Errorf("store: delete named snapshot grants of %s: %w", user, err)
	}
	return nil
}

func (db *DB) queryNamedSnapshotGrants(ctx context.Context, where string, args ...any) ([]NamedSnapshotGrantRow, error) {
	rows, err := db.r.QueryContext(ctx,
		`SELECT owner, name, grantee, created_at FROM named_snapshot_grants `+where, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list named snapshot grants: %w", err)
	}
	defer rows.Close()
	var out []NamedSnapshotGrantRow
	for rows.Next() {
		var g NamedSnapshotGrantRow
		var createdAt string
		if err := rows.Scan(&g.Owner, &g.Name, &g.Grantee, &createdAt); err != nil {
			return nil, fmt.Errorf("store: list named snapshot grants: %w", err)
		}
		g.CreatedAt = parseTime(createdAt)
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list named snapshot grants: %w", err)
	}
	return out, nil
}
