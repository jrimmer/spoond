-- Pins replace holds (FS5, owner decision 2026-10-08): a lease can be
-- pinned by its owner, and spoond never pauses or deletes a pinned lease
-- before its own expiry. The holder/holder_url labels stay as plain
-- labels with no lifecycle effect. The hold columns (hold_set_at,
-- hold_expires_at, hold_ttl) and their automatic rules are removed.
--
-- Every lease with an unexpired hold at migration time becomes pinned, so
-- the 2.9 window's pool workers (held by pool-spawn) can be unpinned by
-- their holder label right after the upgrade.
--
-- paused_at is when the lease was paused (any reason: take-back, its own
-- idle_suspend, POST /pause). One clock releases every paused lease
-- PAUSED_RELEASE_DAYS after it; resuming clears it.
--
-- pinned_idle_since is when a pinned lease's last API activity first
-- passed PINNED_IDLE_NOTICE_DAYS. Visibility only: nothing is paused,
-- unpinned or released because of it. It clears when activity returns.
--
-- paused_expiry_notified marks that the 24 h warning for a paused lease
-- has been emitted, so a restart does not repeat it.
ALTER TABLE leases ADD COLUMN pinned INTEGER NOT NULL DEFAULT 0;
ALTER TABLE leases ADD COLUMN paused_at TEXT NOT NULL DEFAULT '';
ALTER TABLE leases ADD COLUMN pinned_idle_since TEXT NOT NULL DEFAULT '';
ALTER TABLE leases ADD COLUMN paused_expiry_notified INTEGER NOT NULL DEFAULT 0;

-- A live hold becomes a pin. The empty hold_expires_at is the "unset"
-- marker, and RFC3339Nano strings compare correctly with strftime; a hold
-- that already lapsed (hold_expires_at in the past) does not pin.
UPDATE leases SET pinned = 1
 WHERE hold_expires_at <> ''
   AND hold_expires_at > strftime('%Y-%m-%dT%H:%M:%fZ', 'now');

-- The hold columns (hold_set_at, hold_expires_at, hold_ttl) stay in the
-- table, unused and ignored, for one release: dropping a column cannot be
-- made re-runnable on SQLite, and the FS5 code never reads or writes them
-- again. Migration 0008 still creates them, so an older rewind applies
-- cleanly and this UPDATE keeps working.
