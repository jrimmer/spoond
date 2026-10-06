-- Background exec jobs (2.6, #135).
--
-- One row per background job started through the exec API with
-- "background": true. The command runs in the caller's lease; the guest
-- wrapper records stdout, stderr and rc under
-- /var/lib/spoond/jobs/<job_id>/ and this table tracks the outcome.
-- Rows are deleted with their lease (ON DELETE CASCADE) and pruned once
-- exited records are older than JOB_RETENTION_SECS.
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
  -- generation is the lease's continuity generation when the job
  -- started (2.2). A later reconcile that sees the lease on a newer
  -- generation marks the job lost: its memory did not continue.
  generation  INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX lease_jobs_by_lease ON lease_jobs (lease_id, started_at DESC);
CREATE INDEX lease_jobs_running ON lease_jobs (state) WHERE state = 'running';
