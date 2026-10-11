-- Warm sizes (#151 Z3): an image's own vcpu/memory_mb is its default
-- size and is always warm (images.current_build_id). A manifest's
-- warm_sizes builds one more template build per listed preset, and
-- image_sizes records the current one per image per preset with the
-- shape it was built at, so a create at that shape can start from a
-- memory snapshot instead of a cold boot. Every build_id here is a GC
-- root, like images.current_build_id.
CREATE TABLE image_sizes (
  image      TEXT NOT NULL,
  size       TEXT NOT NULL,  -- the preset name (SIZE_PRESETS)
  build_id   TEXT NOT NULL,
  vcpu       INTEGER NOT NULL,
  memory_mb  INTEGER NOT NULL,
  updated_at TEXT NOT NULL,
  PRIMARY KEY (image, size)
);

-- The smallest memory a lease of this image may ask for (manifest
-- min_memory_mb). 0 = unset: the effective minimum is then 512 MiB, or
-- the image's own memory_mb when that is smaller.
ALTER TABLE images ADD COLUMN min_memory_mb INTEGER NOT NULL DEFAULT 0;
