-- Background job max runtime (spoond-wb5).
--
-- A running job is killed and marked exited once it has run for its
-- effective max runtime: a per-job max_runtime_secs (a request may ask
-- for a shorter value, never longer than the host's JOB_MAX_RUNTIME) is
-- stored here as the number of seconds, 0 meaning "no cap" for records
-- written before this column existed.
--
-- reason records why a record left 'running' outside the guest's own rc
-- file: '' for a normal exit, 'timed_out' when the max runtime was
-- spent. It lets the API and the events say that a job was killed by the
-- cap rather than by its command.
ALTER TABLE lease_jobs ADD COLUMN max_runtime_secs INTEGER NOT NULL DEFAULT 0;
ALTER TABLE lease_jobs ADD COLUMN reason TEXT NOT NULL DEFAULT '';
