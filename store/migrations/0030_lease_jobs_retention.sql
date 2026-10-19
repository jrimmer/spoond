-- Per-job output retention (spoond-hwto).
--
-- retention_secs stores a job's effective retention at start: the
-- number of seconds an exited job's record and guest files are kept
-- before the sweeper prunes them. A request may shorten the host's
-- JOB_RETENTION_SECS, never lengthen it, mirroring max_runtime_secs
-- (migration 0020). 0 means the row was written before this column
-- existed (an upgrade keeps pruning those at the host window) or, once
-- a start stamps the effective value, "no per-job shortening".
ALTER TABLE lease_jobs ADD COLUMN retention_secs INTEGER NOT NULL DEFAULT 0;