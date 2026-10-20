-- Background job output cap: where the dropped front ends.
--
-- The output cap (migration 0031) punches a job stream's front out at a
-- block boundary, so the kept tail usually starts mid-line. Whether the
-- dropped front's own last byte was a newline cannot be recovered from
-- the kept tail; the cap records it here when it counts the dropped
-- bytes. The flag belongs to the latest drop and follows the counters'
-- rule: it only changes when the front rises.
ALTER TABLE lease_jobs ADD COLUMN stdout_dropped_ends_with_newline INTEGER NOT NULL DEFAULT 0;
ALTER TABLE lease_jobs ADD COLUMN stderr_dropped_ends_with_newline INTEGER NOT NULL DEFAULT 0;
