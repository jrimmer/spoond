-- Lease history (records A, owner-approved 2026-10-13): one row per ended
-- lease, so all-time records survive the lease row's deletion and the
-- backend journal's rotation. Written at release (outcome released, with
-- the release's peak and reason) and when a lease is marked lost
-- (outcome lost). INSERT OR IGNORE keeps a lost lease's later release
-- from overwriting the loss.
CREATE TABLE lease_history (
  lease_id     TEXT PRIMARY KEY,
  owner        TEXT NOT NULL,          -- owner id (u-…) or legacy consumer name
  image        TEXT NOT NULL,
  memory_mb    INTEGER NOT NULL DEFAULT 0,
  created_at   TEXT NOT NULL,          -- RFC 3339 UTC
  ended_at     TEXT NOT NULL,          -- RFC 3339 UTC
  outcome      TEXT NOT NULL CHECK (outcome IN ('released','lost')),
  reason       TEXT NOT NULL DEFAULT '',
  peak_mib     INTEGER NOT NULL DEFAULT 0,  -- 0 = never measured
  start_ms     INTEGER NOT NULL DEFAULT 0,  -- create-to-grant latency; 0 = unknown
  start_kind   TEXT NOT NULL DEFAULT '',    -- the lease's Start (warm/cold/…); '' = unknown
  max_nap_secs INTEGER NOT NULL DEFAULT 0   -- longest suspend→resume; 0 = none/unknown
);
CREATE INDEX lease_history_ended ON lease_history(ended_at);

-- start_ms stamps the create-to-grant latency on the live lease row,
-- read at release into the history row.
ALTER TABLE leases ADD COLUMN start_ms INTEGER NOT NULL DEFAULT 0;

-- nap_since stamps when the lease's current sleep began — every suspend
-- path (idle, take-back, drain, hand, resume-failed), not just the
-- automatic ones the structured suspension facts (#145 D6) cover. A
-- resume raises max_nap_secs to now − nap_since and clears it; a lease
-- released while suspended keeps the final sleep uncounted. Distinct
-- from suspended_at (migration 0021), which names only an automatic
-- suspend and is cleared by paths (a lost stamp) that must not end a
-- nap.
ALTER TABLE leases ADD COLUMN nap_since TEXT NOT NULL DEFAULT '';

-- max_nap_secs is the longest suspend→resume so far, kept on the live
-- row so a later release's history row carries it.
ALTER TABLE leases ADD COLUMN max_nap_secs INTEGER NOT NULL DEFAULT 0;
