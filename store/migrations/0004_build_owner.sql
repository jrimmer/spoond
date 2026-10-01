ALTER TABLE builds ADD COLUMN owner TEXT NOT NULL DEFAULT '';

CREATE TABLE build_refs (
  build_id     TEXT NOT NULL,
  ref_build_id TEXT NOT NULL,
  PRIMARY KEY (build_id, ref_build_id)
);
CREATE INDEX build_refs_ref ON build_refs(ref_build_id);
