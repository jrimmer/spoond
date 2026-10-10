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
-- marker; julianday() reads RFC3339Nano in true time order (a
-- whole-second "…:00Z" sorts after a same-second fractional
-- "…:00.293Z" in plain string order), so a hold is compared by
-- instant. A hold that already lapsed (hold_expires_at in the past) does
-- not pin.
UPDATE leases SET pinned = 1
 WHERE hold_expires_at <> ''
   AND julianday(hold_expires_at) > julianday('now');

-- A non-persistent lease the conversion pinned keeps its VM past the TTL
-- its hold had already outlived (spoond-k0uz R3-1). In 2.9 a held lease
-- was never TTL-swept while its hold lived, so a holder client (Honey)
-- could keep a ttl+holder lease alive indefinitely by renewing the hold;
-- such a row's expires_at may be long past at upgrade. Extending it to
-- the hold's expiry (only when that is later) gives the owner the window
-- the hold promised; the first 3.0 sweep would otherwise release the VM
-- at once, because the hold no longer protects anything. Pinned
-- persistent rows are not TTL-swept and need no extension. Compared
-- with julianday() for the same reason as the pin conversion above.
UPDATE leases SET expires_at = hold_expires_at
 WHERE pinned = 1
   AND persistent = 0
   AND julianday(hold_expires_at) > julianday(expires_at);

-- Backfill the one clock: every lease already suspended when the upgrade
-- runs is paused as of the migration time, so it gets a fresh
-- PAUSED_RELEASE_DAYS (30 d) from the upgrade — no lease is released
-- sooner than 30 d after the upgrade. A hand/drain/automatic suspend
-- taken before this migration has no paused_at, and without this backfill
-- such a lease would never hit the one clock (spoond-k0uz H3).
UPDATE leases SET paused_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
 WHERE suspended = 1
   AND paused_at = '';

-- Rollback story (spoond-k0uz M6): clear the hold columns once their
-- expiry has become a pin. A 2.9 binary rolled back onto this database
-- re-reads hold_expires_at and would treat an unexpired hold as live
-- again, re-protecting (or re-pausing) leases the admin route just
-- unpinned. Clearing them makes every row read as unheld to 2.9: it
-- TTL-sweeps by expiry and applies no held rules. The columns stay in the
-- table (dropping a column cannot be made re-runnable on SQLite); the
-- FS5 code never reads or writes them.
UPDATE leases SET hold_expires_at = '', hold_set_at = '', hold_ttl = 0;

-- The hold columns (hold_set_at, hold_expires_at, hold_ttl) stay in the
-- table, unused and ignored, for one release. Migration 0008 still creates
-- them, so an older rewind applies cleanly and this UPDATE keeps working.
