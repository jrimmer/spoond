-- Job records that outlive their lease: lease_jobs rows cascade away
-- with the lease row, so once a lease ended nobody could ask spoond how
-- its jobs ended (a guest that died early and released its own lease
-- left no trace). The release copies the lease's job rows here, a job
-- still running becoming lost with reason lease_released and the
-- stderr tail read from the guest before it went. lease_owner is the
-- lease's owner, who may read the rows back through the jobs API; the
-- sweeper prunes them on the job retention window. No foreign key: the
-- lease row is gone.
CREATE TABLE IF NOT EXISTS ended_jobs (
  job_id                           TEXT PRIMARY KEY,
  lease_id                         TEXT NOT NULL,
  lease_owner                      TEXT NOT NULL,
  owner                            TEXT NOT NULL,
  cmd                              TEXT NOT NULL,
  cwd                              TEXT NOT NULL DEFAULT '',
  state                            TEXT NOT NULL,
  exit_code                        INTEGER,
  started_at                       TEXT NOT NULL,
  ended_at                         TEXT,
  stderr_tail                      TEXT NOT NULL DEFAULT '',
  generation                       INTEGER NOT NULL DEFAULT 0,
  max_runtime_secs                 INTEGER NOT NULL DEFAULT 0,
  reason                           TEXT NOT NULL DEFAULT '',
  retention_secs                   INTEGER NOT NULL DEFAULT 0,
  output_max_bytes                 INTEGER NOT NULL DEFAULT 0,
  stdout_dropped_bytes             INTEGER NOT NULL DEFAULT 0,
  stdout_dropped_newlines          INTEGER NOT NULL DEFAULT 0,
  stdout_dropped_ends_with_newline INTEGER NOT NULL DEFAULT 0,
  stderr_dropped_bytes             INTEGER NOT NULL DEFAULT 0,
  stderr_dropped_newlines          INTEGER NOT NULL DEFAULT 0,
  stderr_dropped_ends_with_newline INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS ended_jobs_by_lease ON ended_jobs (lease_id, started_at DESC);
