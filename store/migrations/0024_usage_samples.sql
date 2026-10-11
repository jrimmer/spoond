-- Usage history (#140 U4): one row per owner per sample tick, written by
-- the sampler from the fair-share snapshot every 15 minutes and pruned
-- once older than 7 days. This is an additive CREATE TABLE only; the
-- leases table is never rebuilt.
CREATE TABLE usage_samples (
  owner            TEXT NOT NULL,
  at               TEXT NOT NULL,  -- RFC 3339 with nanoseconds, UTC
  memory_used_mib  INTEGER NOT NULL,
  disk_used_bytes  INTEGER NOT NULL,
  running          INTEGER NOT NULL,
  paused           INTEGER NOT NULL
);

CREATE INDEX usage_samples_owner_at ON usage_samples (owner, at);
