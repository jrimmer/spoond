-- Held-lease limits (2.1, #111 follow-up): a hold expires on its own
-- (it lasts HOLD_TTL_SECS from when it was set or renewed, at most
-- HOLD_TTL_MAX_SECS for an explicit hold_ttl), and the automatic
-- actions on a held lease are recorded on it (last_action /
-- last_action_at, returned by the lease API). Existing leases get
-- empty columns: no hold expiry and no recorded action.
ALTER TABLE leases ADD COLUMN hold_set_at TEXT NOT NULL DEFAULT '';
ALTER TABLE leases ADD COLUMN hold_expires_at TEXT NOT NULL DEFAULT '';
ALTER TABLE leases ADD COLUMN hold_ttl INTEGER NOT NULL DEFAULT 0;
ALTER TABLE leases ADD COLUMN last_action TEXT NOT NULL DEFAULT '';
ALTER TABLE leases ADD COLUMN last_action_at TEXT NOT NULL DEFAULT '';
