package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
	LostAt time.Time
	// LostReason is why the lease was lost (the lost event's detail);
	// "" for a lease lost before the column existed.
	LostReason string
	Drained    bool // paused by the admin drain, resumed by undrain (U10)
	// Pinned marks a lease its owner pinned (FS5, owner decision
	// 2026-10-08): spoond never pauses or deletes a pinned lease before
	// its own expiry. The lease's TTL still applies; a persistent pinned
	// lease stays until the owner releases it. Only the owner (unpin,
	// DELETE) changes a pin.
	Pinned bool
	// PausedAt is when the lease was paused, whatever paused it
	// (take-back, its own idle_suspend, POST /pause). One clock releases
	// a paused lease PAUSED_RELEASE_DAYS after this instant; resuming
	// clears it. Zero = not paused.
	PausedAt time.Time
	// PinnedIdleSince is when a pinned lease's LastActive first passed
	// PINNED_IDLE_NOTICE_DAYS (FS5 visibility only): GET returns it as
	// pinned_idle_since, a lease.pinned_idle event fires once per
	// crossing, and nothing is paused, unpinned or released because of
	// it. Zero = not flagged.
	PinnedIdleSince time.Time
	// PausedExpiryNotified marks that the lease.paused_expiring warning
	// (24 h before a paused lease is released) has been emitted, so a
	// restart does not repeat it.
	PausedExpiryNotified bool
	// Holder and HolderUrl are plain labels naming what holds the lease
	// (a CI job, a person) and a link to it. They have no effect on the
	// lease's lifecycle; only Pinned does. LastAction/LastActionAt record
	// the last automatic action (idle_suspend, preempt, paused_expired).
	Holder, HolderUrl string
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
	// SuspendReason, SuspendPolicyStep, SuspendBuildID and SuspendedAt
	// record why and how an automatic suspend happened (#145 D6): the
	// reason is idle|idle_suspend|hold_lapsed|pressure|preempt, the policy
	// step is the pressure order's step name or "", the build is the
	// pause build, and suspended_at is when. They are cleared on resume;
	// a hand or drain suspend leaves them empty (no automatic reason).
	SuspendReason     string
	SuspendPolicyStep string
	SuspendBuildID    string
	SuspendedAt       time.Time
}

const leaseColumns = `id, owner, image, sandbox_id, address, created_at, expires_at,
	persistent, last_active, workspace, suspended, name, net_policy, net_allow,
	expose_ports, exposed_ip, comment, state, resume_build_id,
	last_checkpoint_build_id, last_checkpoint_at, recovered_from, drained, lost_at,
	lost_reason,
	pinned, paused_at, pinned_idle_since, paused_expiry_notified,
	holder, holder_url,
	last_action, last_action_at, generation, checkpoint_interval, idle_suspend,
	memory_mb, class, priority, preempted_at, snapshot_build_id,
	suspend_reason, suspend_policy_step, suspend_build_id, suspended_at`

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
  ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
  ?, ?, ?, ?, ?, ?
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
  lost_reason=excluded.lost_reason,
  pinned=excluded.pinned,
  paused_at=excluded.paused_at,
  pinned_idle_since=excluded.pinned_idle_since,
  paused_expiry_notified=excluded.paused_expiry_notified,
  holder=excluded.holder,
  holder_url=excluded.holder_url,
  last_action=excluded.last_action,
  last_action_at=excluded.last_action_at,
  generation=excluded.generation,
  checkpoint_interval=excluded.checkpoint_interval,
  idle_suspend=excluded.idle_suspend,
  memory_mb=excluded.memory_mb,
  class=excluded.class,
  priority=excluded.priority,
  preempted_at=excluded.preempted_at,
  snapshot_build_id=excluded.snapshot_build_id,
  suspend_reason=excluded.suspend_reason,
  suspend_policy_step=excluded.suspend_policy_step,
  suspend_build_id=excluded.suspend_build_id,
  suspended_at=excluded.suspended_at`,
		l.ID, l.Owner, l.Image, l.SandboxID, l.Address,
		formatTime(l.CreatedAt), formatTime(l.ExpiresAt), l.Persistent,
		formatTime(l.LastActive), l.Workspace, l.Suspended, l.Name,
		l.NetPolicy, string(netAllow), string(exposePorts), l.ExposedIP,
		l.Comment, l.State, l.ResumeBuildID, l.LastCheckpointBuildID,
		formatTime(l.LastCheckpointAt), formatTime(l.RecoveredFrom), l.Drained,
		formatTime(l.LostAt), l.LostReason, l.Pinned, formatTime(l.PausedAt),
		formatTime(l.PinnedIdleSince), l.PausedExpiryNotified,
		l.Holder, l.HolderUrl,
		l.LastAction, formatTime(l.LastActionAt), l.Generation,
		l.CheckpointInterval, l.IdleSuspend, l.MemoryMB, l.Class, l.Priority,
		formatTime(l.PreemptedAt), l.SnapshotBuildID,
		l.SuspendReason, l.SuspendPolicyStep, l.SuspendBuildID,
		formatTime(l.SuspendedAt))
	if err != nil {
		return fmt.Errorf("store: upsert lease %s: %w", l.ID, err)
	}
	return nil
}

// UpdateLeaseLostAt stamps one lease row's lost_at without touching any
// other column. The GC uses it for a stored-only lost lease (no
// in-memory twin), where an UpsertLease would rewrite every column and
// could re-insert a row a concurrent release had just deleted
// (spoond-d76). A missing row is not an error: the release won the race.
// The WHERE clause's empty-lost_at guard means an existing stamp is never
// overwritten either (spoond-15i).
func (db *DB) UpdateLeaseLostAt(ctx context.Context, id string, lostAt time.Time) error {
	_, err := db.w.ExecContext(ctx,
		`UPDATE leases SET lost_at = ? WHERE id = ? AND lost_at = ''`, formatTime(lostAt), id)
	if err != nil {
		return fmt.Errorf("store: update lost_at %s: %w", id, err)
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

// UnpinLeasesByHolderPrefix clears the pinned flag of every lease whose
// holder starts with prefix, returning how many rows changed. It exists
// for the 2.9→3.0 migration window: migration 0022 turns every live
// hold into a pin, and pool-spawn held its worker leases with a holder
// label (e.g. "pool:"), so an operator can unpin exactly those. An
// empty prefix refuses (it would unpin every lease).
func (db *DB) UnpinLeasesByHolderPrefix(ctx context.Context, prefix string) (int64, error) {
	if prefix == "" {
		return 0, fmt.Errorf("store: unpin by holder prefix: prefix is empty")
	}
	res, err := db.w.ExecContext(ctx,
		`UPDATE leases SET pinned = 0 WHERE pinned = 1 AND holder LIKE ? ESCAPE '\\'`,
		escapeLikePrefix(prefix)+"%")
	if err != nil {
		return 0, fmt.Errorf("store: unpin leases by holder prefix %q: %w", prefix, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: unpin leases by holder prefix %q: %w", prefix, err)
	}
	return n, nil
}

// escapeLikePrefix escapes the LIKE wildcards in a literal prefix so it
// matches only itself; the caller appends "%" and uses ESCAPE '\'.
func escapeLikePrefix(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
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
	var pausedAt, pinnedIdleSince, lastActionAt, preemptedAt string
	var suspendedAt string
	var netAllow, exposePorts string
	err := scan(&r.ID, &r.Owner, &r.Image, &r.SandboxID, &r.Address,
		&createdAt, &expiresAt, &r.Persistent, &lastActive, &r.Workspace,
		&r.Suspended, &r.Name, &r.NetPolicy, &netAllow, &exposePorts,
		&r.ExposedIP, &r.Comment, &r.State, &r.ResumeBuildID,
		&r.LastCheckpointBuildID, &lastCheckpointAt, &recoveredFrom, &r.Drained,
		&lostAt, &r.LostReason, &r.Pinned, &pausedAt,
		&pinnedIdleSince, &r.PausedExpiryNotified, &r.Holder, &r.HolderUrl,
		&r.LastAction, &lastActionAt, &r.Generation, &r.CheckpointInterval,
		&r.IdleSuspend, &r.MemoryMB, &r.Class, &r.Priority, &preemptedAt,
		&r.SnapshotBuildID, &r.SuspendReason, &r.SuspendPolicyStep,
		&r.SuspendBuildID, &suspendedAt)
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
	r.PausedAt = parseTime(pausedAt)
	r.PinnedIdleSince = parseTime(pinnedIdleSince)
	r.LastActionAt = parseTime(lastActionAt)
	r.PreemptedAt = parseTime(preemptedAt)
	r.SuspendedAt = parseTime(suspendedAt)
	if err := json.Unmarshal([]byte(netAllow), &r.NetAllow); err != nil {
		return LeaseRow{}, fmt.Errorf("store: lease %s: net_allow: %w", r.ID, err)
	}
	if err := json.Unmarshal([]byte(exposePorts), &r.ExposePorts); err != nil {
		return LeaseRow{}, fmt.Errorf("store: lease %s: expose_ports: %w", r.ID, err)
	}
	return r, nil
}
