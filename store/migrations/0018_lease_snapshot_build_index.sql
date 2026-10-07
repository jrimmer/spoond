-- Index the lease -> named-snapshot-version link (2.7, #83 N5).
-- Retention, the delete route and the list route all look leases up by
-- the version build they started from; without this index every such
-- check scans the leases table.
CREATE INDEX IF NOT EXISTS leases_snapshot_build_id ON leases(snapshot_build_id);
