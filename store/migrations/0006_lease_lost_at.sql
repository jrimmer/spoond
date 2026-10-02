-- When the lease became lost (RFC3339Nano, '' = unset). A lease that
-- was already lost when this column was added has '' and counts as
-- lost at the time of the GC pass that first sees it, so its snapshot
-- grace period starts then.
ALTER TABLE leases ADD COLUMN lost_at TEXT NOT NULL DEFAULT '';
