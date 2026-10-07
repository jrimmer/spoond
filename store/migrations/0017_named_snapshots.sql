-- Named snapshots (2.7, #83): a checkpoint build with a stable name and a
-- version, owned by an identity and outliving the lease it was saved
-- from. One row per (owner, name, version); the build_id is the
-- checkpoint build the version points at. Rows are GC roots until
-- deleted.
CREATE TABLE named_snapshots (
  owner                TEXT NOT NULL,
  name                 TEXT NOT NULL,
  version              INTEGER NOT NULL,
  build_id             TEXT NOT NULL UNIQUE,
  idempotency_key      TEXT NOT NULL DEFAULT '',
  source_lease_id      TEXT NOT NULL,
  image                TEXT NOT NULL,
  image_build_id       TEXT NOT NULL,  -- the image (template) build at the root of the chain
  memory_mb            INTEGER NOT NULL,
  size_bytes           INTEGER NOT NULL,
  envd_version         TEXT NOT NULL,
  firecracker_version  TEXT NOT NULL,
  orchestrator_version TEXT NOT NULL DEFAULT '',
  created_at           TEXT NOT NULL,
  PRIMARY KEY (owner, name, version)
);
CREATE UNIQUE INDEX named_snapshots_key ON named_snapshots(owner, name, idempotency_key) WHERE idempotency_key <> '';

-- Per-name retention (2.7, #83): how many versions a name keeps. Written
-- by the first save, changed by PUT /api/named-snapshots/{name}; deleted
-- with the name's last version.
CREATE TABLE named_snapshot_names (
  owner      TEXT NOT NULL,
  name       TEXT NOT NULL,
  keep       INTEGER NOT NULL,
  created_at TEXT NOT NULL,
  PRIMARY KEY (owner, name)
);

-- The named snapshot a lease was started from (2.7, #83 task 2): the
-- version's build id, '' when the lease did not start from a snapshot.
-- Retention must never drop a version a live lease still runs from.
ALTER TABLE leases ADD COLUMN snapshot_build_id TEXT NOT NULL DEFAULT '';
