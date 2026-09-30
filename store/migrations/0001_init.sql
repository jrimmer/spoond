CREATE TABLE schema_migrations (
  version    INTEGER PRIMARY KEY,
  applied_at TEXT NOT NULL
);

CREATE TABLE leases (
  id                        TEXT PRIMARY KEY,
  owner                     TEXT NOT NULL,
  image                     TEXT NOT NULL,
  sandbox_id                TEXT NOT NULL DEFAULT '',
  address                   TEXT NOT NULL DEFAULT '',
  created_at                TEXT NOT NULL,
  expires_at                TEXT NOT NULL,
  persistent                INTEGER NOT NULL DEFAULT 0,
  last_active               TEXT NOT NULL,
  workspace                 TEXT NOT NULL DEFAULT '',
  suspended                 INTEGER NOT NULL DEFAULT 0,
  name                      TEXT NOT NULL DEFAULT '',
  net_policy                TEXT NOT NULL DEFAULT '',
  net_allow                 TEXT NOT NULL DEFAULT '[]',
  expose_ports              TEXT NOT NULL DEFAULT '[]',
  exposed_ip                TEXT NOT NULL DEFAULT '',
  comment                   TEXT NOT NULL DEFAULT '',
  state                     TEXT NOT NULL DEFAULT 'running'
                              CHECK (state IN ('running','suspended','recovered','lost')),
  resume_build_id           TEXT NOT NULL DEFAULT '',
  last_checkpoint_build_id  TEXT NOT NULL DEFAULT '',
  last_checkpoint_at        TEXT NOT NULL DEFAULT '',
  recovered_from            TEXT NOT NULL DEFAULT ''
);
CREATE INDEX leases_owner ON leases(owner);
CREATE UNIQUE INDEX leases_owner_name ON leases(owner, name) WHERE name <> '';

CREATE TABLE shares (
  lease_id   TEXT NOT NULL REFERENCES leases(id) ON DELETE CASCADE,
  grantee    TEXT NOT NULL,
  mode       TEXT NOT NULL CHECK (mode IN ('ssh','http')),
  expires_at TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  PRIMARY KEY (lease_id, grantee)
);

CREATE TABLE pool (
  sandbox_id TEXT PRIMARY KEY,
  image      TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE INDEX pool_image ON pool(image);

CREATE TABLE images (
  name             TEXT PRIMARY KEY,
  template_id      TEXT NOT NULL,
  current_build_id TEXT NOT NULL DEFAULT '',
  digest           TEXT NOT NULL DEFAULT '',
  vcpu             INTEGER NOT NULL,
  memory_mb        INTEGER NOT NULL,
  disk_mb          INTEGER NOT NULL,
  start_cmd        TEXT NOT NULL DEFAULT '',
  ready_cmd        TEXT NOT NULL DEFAULT '',
  updated_at       TEXT NOT NULL
);

CREATE TABLE builds (
  build_id            TEXT PRIMARY KEY,
  kind                TEXT NOT NULL CHECK (kind IN ('template','pause','checkpoint')),
  template_id         TEXT NOT NULL,
  image               TEXT NOT NULL,
  parent_build_id     TEXT NOT NULL DEFAULT '',
  source_sandbox_id   TEXT NOT NULL DEFAULT '',
  state               TEXT NOT NULL CHECK (state IN ('building','ready','failed','deleted')),
  kernel_version      TEXT NOT NULL DEFAULT '',
  firecracker_version TEXT NOT NULL DEFAULT '',
  envd_version        TEXT NOT NULL DEFAULT '',
  vcpu                INTEGER NOT NULL DEFAULT 0,
  memory_mb           INTEGER NOT NULL DEFAULT 0,
  disk_mb             INTEGER NOT NULL DEFAULT 0,
  size_bytes          INTEGER NOT NULL DEFAULT 0,
  error               TEXT NOT NULL DEFAULT '',
  created_at          TEXT NOT NULL,
  updated_at          TEXT NOT NULL
);
CREATE INDEX builds_parent ON builds(parent_build_id);
CREATE INDEX builds_image ON builds(image);

CREATE TABLE sandboxes (
  sandbox_id   TEXT PRIMARY KEY,
  lease_id     TEXT NOT NULL DEFAULT '',
  build_id     TEXT NOT NULL,
  execution_id TEXT NOT NULL,
  host_ip      TEXT NOT NULL DEFAULT '',
  vcpu         INTEGER NOT NULL,
  memory_mb    INTEGER NOT NULL,
  started_at   TEXT NOT NULL,
  end_at       TEXT NOT NULL
);
CREATE INDEX sandboxes_lease ON sandboxes(lease_id);
