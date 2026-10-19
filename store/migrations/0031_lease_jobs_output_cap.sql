-- Background job output cap (spoond-hwto).
--
-- output_max_bytes stores a job's effective per-stream output cap at
-- start (in bytes): once a guest stdout or stderr file grows past it,
-- the job poller punches the file's front out (fallocate --punch-hole
-- in the guest, block-aligned) and keeps only the tail. A request may shorten the host's JOB_OUTPUT_MAX_BYTES,
-- never lengthen it, mirroring max_runtime_secs (migration 0020) and
-- retention_secs (migration 0030). 0 means "no host default" and is
-- also what records written before this column existed hold: an
-- upgrade caps nothing until a start asks or the host default is set.
--
-- The <stream>_dropped_* counters are absolute: they are how many bytes
-- of the stream's front have been punched out (and how many newlines
-- those bytes held, counted before each punch), so a reader can map an
-- absolute offset onto the file. The counters only ever rise; the MAX()
-- in the update keeps a guest sidecar rolled back by a snapshot
-- restore from shortening what the record reports.
ALTER TABLE lease_jobs ADD COLUMN output_max_bytes INTEGER NOT NULL DEFAULT 0;
ALTER TABLE lease_jobs ADD COLUMN stdout_dropped_bytes INTEGER NOT NULL DEFAULT 0;
ALTER TABLE lease_jobs ADD COLUMN stdout_dropped_newlines INTEGER NOT NULL DEFAULT 0;
ALTER TABLE lease_jobs ADD COLUMN stderr_dropped_bytes INTEGER NOT NULL DEFAULT 0;
ALTER TABLE lease_jobs ADD COLUMN stderr_dropped_newlines INTEGER NOT NULL DEFAULT 0;
