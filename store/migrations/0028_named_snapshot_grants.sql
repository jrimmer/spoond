-- Named-snapshot grants (#141): an owner lets another user start leases
-- from one of its names. One row per (owner, name, grantee); a grant
-- covers every version of the name, and only secret-free versions can
-- be started through it. Revoking deletes the row and never touches a
-- running lease.
CREATE TABLE named_snapshot_grants (
  owner      TEXT NOT NULL,
  name       TEXT NOT NULL,
  grantee    TEXT NOT NULL,
  created_at TEXT NOT NULL,
  PRIMARY KEY (owner, name, grantee)
);
CREATE INDEX named_snapshot_grants_grantee ON named_snapshot_grants(grantee);

-- had_secrets marks a lease that ever held a secret: create-time,
-- exec-time or job secrets, or one inherited from the lease or tainted
-- version it was cloned, forked or started from. It is never cleared.
-- Existing leases read 0; their saves before this migration stay
-- ungrantable anyway (below).
ALTER TABLE leases ADD COLUMN had_secrets INTEGER NOT NULL DEFAULT 0;

-- secret_free marks a version saved from a lease that never had a
-- secret. Existing versions read 0 (not grantable until re-saved): their
-- source's history is unknown, so the conservative answer is "tainted".
ALTER TABLE named_snapshots ADD COLUMN secret_free INTEGER NOT NULL DEFAULT 0;
