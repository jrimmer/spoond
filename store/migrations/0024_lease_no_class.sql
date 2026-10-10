-- Fair shares 3.0 (spoond-pxsn FS2b-1): the guaranteed/burst admission
-- classes are gone. Every lease admits against the one hugepage pool and
-- take-back (pausing the biggest borrower's leases) makes room; there is
-- no class, no scheduling priority and no preemption stamp any more.
--
-- The class column carries a CHECK (class IN ('guaranteed','burst'))
-- that 0013 added, so the column cannot simply be dropped or left
-- writable: the leases table is rebuilt without the check, keeping the
-- class column (always 'guaranteed' from here on) so the store layer
-- keeps one row shape. Because the rename rewrites the REFERENCES
-- clauses of the child tables while foreign keys are on, the three
-- tables that point at leases (shares, lease_kept_builds, lease_jobs)
-- are rebuilt in the same transaction. Every row is preserved; the live
-- preempted_at values are cleared (they named a lease waiting for the
-- old preemption resume queue, which no longer exists).
ALTER TABLE leases RENAME TO leases_old;
ALTER TABLE shares RENAME TO shares_old;
ALTER TABLE lease_kept_builds RENAME TO lease_kept_builds_old;
ALTER TABLE lease_jobs RENAME TO lease_jobs_old;

CREATE TABLE leases (
  id                        TEXT PRIMARY KEY,
  owner                     TEXT NOT NULL,
  image                     TEXT NOT NULL,
  sandbox_id                TEXT NOT NULL DEFAULT '',
  address                   TEXT NOT NULL DEFAULT '',
  created_at                TEXT NOT NULL,
  expires_at                TEXT NOT NULL,
  persistent                INTEGER NOT NULL DEFAULT 0,
  last_active               TEXT NOT NULL,
  workspace                 TEXT NOT NULL DEFAULT '',
  suspended                 INTEGER NOT NULL DEFAULT 0,
  name                      TEXT NOT NULL DEFAULT '',
  net_policy                TEXT NOT NULL DEFAULT '',
  net_allow                 TEXT NOT NULL DEFAULT '[]',
  expose_ports              TEXT NOT NULL DEFAULT '[]',
  exposed_ip                TEXT NOT NULL DEFAULT '',
  comment                   TEXT NOT NULL DEFAULT '',
  state                     TEXT NOT NULL DEFAULT 'running'
                              CHECK (state IN ('running','suspended','recovered','lost')),
  resume_build_id           TEXT NOT NULL DEFAULT '',
  last_checkpoint_build_id  TEXT NOT NULL DEFAULT '',
  last_checkpoint_at        TEXT NOT NULL DEFAULT '',
  recovered_from            TEXT NOT NULL DEFAULT '',
  drained                   INTEGER NOT NULL DEFAULT 0,
  lost_at                   TEXT NOT NULL DEFAULT '',
  holder                    TEXT NOT NULL DEFAULT '',
  holder_url                TEXT NOT NULL DEFAULT '',
  hold_set_at               TEXT NOT NULL DEFAULT '',
  hold_expires_at           TEXT NOT NULL DEFAULT '',
  hold_ttl                  INTEGER NOT NULL DEFAULT 0,
  last_action               TEXT NOT NULL DEFAULT '',
  last_action_at            TEXT NOT NULL DEFAULT '',
  generation                INTEGER NOT NULL DEFAULT 1,
  checkpoint_interval       INTEGER NOT NULL DEFAULT -1,
  memory_mb                 INTEGER NOT NULL DEFAULT 0,
  class                     TEXT NOT NULL DEFAULT 'guaranteed',
  priority                  INTEGER NOT NULL DEFAULT 0,
  preempted_at              TEXT NOT NULL DEFAULT '',
  idle_suspend              INTEGER NOT NULL DEFAULT -1,
  snapshot_build_id         TEXT NOT NULL DEFAULT '',
  lost_reason               TEXT NOT NULL DEFAULT '',
  suspend_reason            TEXT NOT NULL DEFAULT '',
  suspend_policy_step       TEXT NOT NULL DEFAULT '',
  suspend_build_id          TEXT NOT NULL DEFAULT '',
  suspended_at              TEXT NOT NULL DEFAULT '',
  pinned                    INTEGER NOT NULL DEFAULT 0,
  paused_at                 TEXT NOT NULL DEFAULT '',
  pinned_idle_since         TEXT NOT NULL DEFAULT '',
  paused_expiry_notified    INTEGER NOT NULL DEFAULT 0,
  disk_mb                   INTEGER NOT NULL DEFAULT 0
);

INSERT INTO leases SELECT * FROM leases_old;

CREATE TABLE shares (
  lease_id   TEXT NOT NULL REFERENCES leases(id) ON DELETE CASCADE,
  grantee    TEXT NOT NULL,
  mode       TEXT NOT NULL CHECK (mode IN ('ssh','http')),
  expires_at TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  PRIMARY KEY (lease_id, grantee)
);
INSERT INTO shares SELECT * FROM shares_old;
DROP TABLE shares_old;

CREATE TABLE lease_kept_builds (
  lease_id  TEXT NOT NULL REFERENCES leases(id) ON DELETE CASCADE,
  build_id  TEXT NOT NULL,
  kept_at   TEXT NOT NULL,
  PRIMARY KEY (lease_id, build_id)
);
INSERT INTO lease_kept_builds SELECT * FROM lease_kept_builds_old;
DROP TABLE lease_kept_builds_old;

CREATE TABLE lease_jobs (
  job_id      TEXT PRIMARY KEY,
  lease_id    TEXT NOT NULL REFERENCES leases(id) ON DELETE CASCADE,
  owner       TEXT NOT NULL,
  cmd         TEXT NOT NULL,
  cwd         TEXT NOT NULL DEFAULT '',
  state       TEXT NOT NULL,           -- running | exited | lost
  exit_code   INTEGER,                 -- NULL while running
  started_at  TEXT NOT NULL,
  ended_at    TEXT,                    -- '' / NULL while running
  stderr_tail TEXT NOT NULL DEFAULT '',-- last 4 KiB of stderr
  generation  INTEGER NOT NULL DEFAULT 0,
  max_runtime_secs INTEGER NOT NULL DEFAULT 0,
  reason      TEXT NOT NULL DEFAULT ''
);
INSERT INTO lease_jobs SELECT * FROM lease_jobs_old;
DROP TABLE lease_jobs_old;

DROP TABLE leases_old;

CREATE INDEX leases_owner ON leases(owner);
CREATE UNIQUE INDEX leases_owner_name ON leases(owner, name) WHERE name <> '';
CREATE INDEX leases_snapshot_build_id ON leases(snapshot_build_id);
CREATE INDEX lease_kept_builds_build ON lease_kept_builds(build_id);
CREATE INDEX lease_jobs_by_lease ON lease_jobs (lease_id, started_at DESC);
CREATE INDEX lease_jobs_running ON lease_jobs (state) WHERE state = 'running';

UPDATE leases SET preempted_at = '' WHERE preempted_at <> '';
