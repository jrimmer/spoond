package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// LeaseRow is one row of the leases table.
type LeaseRow struct {
	ID, Owner, Image, SandboxID, Address string
	CreatedAt, ExpiresAt, LastActive     time.Time
	Persistent, Suspended                bool
	Workspace, Name, NetPolicy           string
	NetAllow                             []string // JSON in net_allow
	ExposePorts                          []int    // JSON in expose_ports
	ExposedIP, Comment, State            string   // State: running|suspended|recovered|lost
	ResumeBuildID, LastCheckpointBuildID string
	LastCheckpointAt, RecoveredFrom      time.Time // zero = unset ('')
	// LostAt is when the lease became lost (zero = unset). The GC keeps
	// a lost lease's snapshot builds for a grace period counted from it.
	LostAt  time.Time
	Drained bool // paused by the admin drain, resumed by undrain (U10)
	// Holder names what holds the lease (a CI job, a person) and
	// HolderUrl links to it. A non-empty holder keeps the lease out of
	// every sweeper (TTL, idle); checkpoints follow checkpoint_interval.
	// HoldSetAt/HoldExpiresAt bound the hold (2.1): past HoldExpiresAt
	// the holder is cleared and normal sweeping resumes. HoldTTL is the
	// requested explicit hold_ttl in seconds (0 = the default). The
	// Last* fields record the last automatic held-lease action.
	Holder, HolderUrl string
	HoldSetAt         time.Time
	HoldExpiresAt     time.Time
	HoldTTL           int64
	LastAction        string
	LastActionAt      time.Time
	// Generation is the lease's current continuity generation (2.2):
	// bumped every time the guest's memory does not continue from where
	// its processes left it (crash recovery, restart), left alone across
	// a planned pause/resume or drain/undrain (the memory continues).
	// Starts at 1 on create.
	Generation int64
	// CheckpointInterval is the lease's own periodic checkpoint
	// interval in seconds (2.3, #122): -1 = the host default
	// (CHECKPOINT_INTERVAL_MINS, itself 0 = never), 0 = never,
	// >0 = seconds.
	CheckpointInterval int64
	// IdleSuspend is the lease's own idle reclamation threshold in
	// seconds (2.5, #129 part 2): -1 = the host default
	// (IDLE_SUSPEND_DEFAULT_SECS, itself 0 = never), 0 = never, >0 =
	// suspend the persistent lease after that long without activity.
	IdleSuspend int64
	// MemoryMB is the image's memory_mb stamped when the lease was
	// granted (#128): the lease's MiB charge while it runs. Cached on
	// the row so quota accounting never reads the image catalog under
	// the lease store's lock; 0 = unknown (the image row is gone).
	MemoryMB int
	// Class is the lease's admission class (#128 part 2):
	// "guaranteed" while the owner's running charge stays within their
	// guaranteed_mib, "burst" above it (or when the request forced
	// burst). Priority orders preemption within a class: a lower
	// number is preempted first, 0 the default.
	Class    string
	Priority int
	// PreemptedAt is when the lease was preempted (#128 part 3): it was
	// suspended to free hugepages for a guaranteed admission, and the
	// resume queue brings it back when capacity allows. Zero = not
	// preempted. Cleared on resume.
	PreemptedAt time.Time
	// SnapshotBuildID is the named-snapshot version's build the lease
	// started from (2.7, #83); '' when it did not start from one. A
	// version whose build a live lease runs from is never dropped by
	// retention.
	SnapshotBuildID string
}

const leaseColumns = `id, owner, image, sandbox_id, address, created_at, expires_at,
	persistent, last_active, workspace, suspended, name, net_policy, net_allow,
	expose_ports, exposed_ip, comment, state, resume_build_id,
	last_checkpoint_build_id, last_checkpoint_at, recovered_from, drained, lost_at,
	holder, holder_url, hold_set_at, hold_expires_at, hold_ttl,
	last_action, last_action_at, generation, checkpoint_interval, idle_suspend,
	memory_mb, class, priority, preempted_at, snapshot_build_id`

// UpsertLease inserts the lease row, or updates every column of the
// existing row when the id already exists.
func (db *DB) UpsertLease(ctx context.Context, l LeaseRow) error {
	netAllow, err := json.Marshal(l.NetAllow)
	if err != nil {
		return fmt.Errorf("store: lease %s: net_allow: %w", l.ID, err)
	}
	exposePorts, err := json.Marshal(l.ExposePorts)
	if err != nil {
		return fmt.Errorf("store: lease %s: expose_ports: %w", l.ID, err)
	}
	_, err = db.w.ExecContext(ctx, `
INSERT INTO leases (`+leaseColumns+`) VALUES (
  ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
  ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
  ?
)
ON CONFLICT(id) DO UPDATE SET
  owner=excluded.owner,
  image=excluded.image,
  sandbox_id=excluded.sandbox_id,
  address=excluded.address,
  created_at=excluded.created_at,
  expires_at=excluded.expires_at,
  persistent=excluded.persistent,
  last_active=excluded.last_active,
  workspace=excluded.workspace,
  suspended=excluded.suspended,
  name=excluded.name,
  net_policy=excluded.net_policy,
  net_allow=excluded.net_allow,
  expose_ports=excluded.expose_ports,
  exposed_ip=excluded.exposed_ip,
  comment=excluded.comment,
  state=excluded.state,
  resume_build_id=excluded.resume_build_id,
  last_checkpoint_build_id=excluded.last_checkpoint_build_id,
  last_checkpoint_at=excluded.last_checkpoint_at,
  recovered_from=excluded.recovered_from,
  drained=excluded.drained,
  lost_at=excluded.lost_at,
  holder=excluded.holder,
  holder_url=excluded.holder_url,
  hold_set_at=excluded.hold_set_at,
  hold_expires_at=excluded.hold_expires_at,
  hold_ttl=excluded.hold_ttl,
  last_action=excluded.last_action,
  last_action_at=excluded.last_action_at,
  generation=excluded.generation,
  checkpoint_interval=excluded.checkpoint_interval,
  idle_suspend=excluded.idle_suspend,
  memory_mb=excluded.memory_mb,
  class=excluded.class,
  priority=excluded.priority,
  preempted_at=excluded.preempted_at,
  snapshot_build_id=excluded.snapshot_build_id`,
		l.ID, l.Owner, l.Image, l.SandboxID, l.Address,
		formatTime(l.CreatedAt), formatTime(l.ExpiresAt), l.Persistent,
		formatTime(l.LastActive), l.Workspace, l.Suspended, l.Name,
		l.NetPolicy, string(netAllow), string(exposePorts), l.ExposedIP,
		l.Comment, l.State, l.ResumeBuildID, l.LastCheckpointBuildID,
		formatTime(l.LastCheckpointAt), formatTime(l.RecoveredFrom), l.Drained,
		formatTime(l.LostAt), l.Holder, l.HolderUrl,
		formatTime(l.HoldSetAt), formatTime(l.HoldExpiresAt), l.HoldTTL,
		l.LastAction, formatTime(l.LastActionAt), l.Generation,
		l.CheckpointInterval, l.IdleSuspend, l.MemoryMB, l.Class, l.Priority,
		formatTime(l.PreemptedAt), l.SnapshotBuildID)
	if err != nil {
		return fmt.Errorf("store: upsert lease %s: %w", l.ID, err)
	}
	return nil
}

// DeleteLease removes the lease row; its shares cascade via foreign key.
func (db *DB) DeleteLease(ctx context.Context, id string) error {
	_, err := db.w.ExecContext(ctx, `DELETE FROM leases WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete lease %s: %w", id, err)
	}
	return nil
}

// GetLease returns one lease row by id (store.ErrNotFound when the id
// is unknown).
func (db *DB) GetLease(ctx context.Context, id string) (LeaseRow, error) {
	row, err := scanLease(db.r.QueryRowContext(ctx,
		`SELECT `+leaseColumns+` FROM leases WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return LeaseRow{}, ErrNotFound
	}
	if err != nil {
		return LeaseRow{}, fmt.Errorf("store: get lease %s: %w", id, err)
	}
	return row, nil
}

// ListLeases returns every lease row, ordered by id.
func (db *DB) ListLeases(ctx context.Context) ([]LeaseRow, error) {
	rows, err := db.r.QueryContext(ctx, `SELECT `+leaseColumns+` FROM leases ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: list leases: %w", err)
	}
	defer rows.Close()
	var out []LeaseRow
	for rows.Next() {
		r, err := scanLease(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("store: list leases: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list leases: %w", err)
	}
	return out, nil
}

// UpdateLastActive writes the given LastActive timestamps in one
// transaction.
func (db *DB) UpdateLastActive(ctx context.Context, ids map[string]time.Time) error {
	tx, err := db.w.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: update last_active: %w", err)
	}
	for id, t := range ids {
		if _, err := tx.ExecContext(ctx,
			`UPDATE leases SET last_active = ? WHERE id = ?`, formatTime(t), id); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: update last_active %s: %w", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: update last_active: %w", err)
	}
	return nil
}

// scanLease scans one leases row (in leaseColumns order) into a
// LeaseRow, decoding the timestamp and JSON columns.
func scanLease(scan func(dest ...any) error) (LeaseRow, error) {
	var r LeaseRow
	var createdAt, expiresAt, lastActive, lastCheckpointAt, recoveredFrom, lostAt string
	var holdSetAt, holdExpiresAt, lastActionAt, preemptedAt string
	var netAllow, exposePorts string
	err := scan(&r.ID, &r.Owner, &r.Image, &r.SandboxID, &r.Address,
		&createdAt, &expiresAt, &r.Persistent, &lastActive, &r.Workspace,
		&r.Suspended, &r.Name, &r.NetPolicy, &netAllow, &exposePorts,
		&r.ExposedIP, &r.Comment, &r.State, &r.ResumeBuildID,
		&r.LastCheckpointBuildID, &lastCheckpointAt, &recoveredFrom, &r.Drained,
		&lostAt, &r.Holder, &r.HolderUrl,
		&holdSetAt, &holdExpiresAt, &r.HoldTTL,
		&r.LastAction, &lastActionAt, &r.Generation, &r.CheckpointInterval,
		&r.IdleSuspend, &r.MemoryMB, &r.Class, &r.Priority, &preemptedAt,
		&r.SnapshotBuildID)
	if errors.Is(err, sql.ErrNoRows) {
		return LeaseRow{}, ErrNotFound
	}
	if err != nil {
		return LeaseRow{}, err
	}
	r.CreatedAt = parseTime(createdAt)
	r.ExpiresAt = parseTime(expiresAt)
	r.LastActive = parseTime(lastActive)
	r.LastCheckpointAt = parseTime(lastCheckpointAt)
	r.RecoveredFrom = parseTime(recoveredFrom)
	r.LostAt = parseTime(lostAt)
	r.HoldSetAt = parseTime(holdSetAt)
	r.HoldExpiresAt = parseTime(holdExpiresAt)
	r.LastActionAt = parseTime(lastActionAt)
	r.PreemptedAt = parseTime(preemptedAt)
	if err := json.Unmarshal([]byte(netAllow), &r.NetAllow); err != nil {
		return LeaseRow{}, fmt.Errorf("store: lease %s: net_allow: %w", r.ID, err)
	}
	if err := json.Unmarshal([]byte(exposePorts), &r.ExposePorts); err != nil {
		return LeaseRow{}, fmt.Errorf("store: lease %s: expose_ports: %w", r.ID, err)
	}
	return r, nil
}
