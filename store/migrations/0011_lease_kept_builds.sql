-- Kept checkpoints (2.3, #121): a lease can pin a checkpoint build so
-- it survives GC while the lease lives and can be restored in place.
-- One row per kept build, keyed (lease_id, build_id): a lease keeps
-- several builds (every checkpoint it pinned), a build is pinned by the
-- lease that checkpointed it. The rows go when the lease does (ON
-- DELETE CASCADE) or when the lease is released, so the next GC pass
-- may reclaim the builds.
CREATE TABLE lease_kept_builds (
  lease_id  TEXT NOT NULL REFERENCES leases(id) ON DELETE CASCADE,
  build_id  TEXT NOT NULL,
  kept_at   TEXT NOT NULL,
  PRIMARY KEY (lease_id, build_id)
);
CREATE INDEX lease_kept_builds_build ON lease_kept_builds(build_id);
