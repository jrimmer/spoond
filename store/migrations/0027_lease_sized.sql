-- Per-lease sizes (#151 Z4): sized marks a lease created at a size its
-- caller chose (size, vcpu or memory_mb). A sized lease keeps that size
-- through every restart; any other lease follows its image's current
-- default size when a restart boots a fresh guest, as before 3.1.
-- Clones and forks inherit it. Every existing lease took its image's
-- default, so the default 0 is right for all of them.
ALTER TABLE leases ADD COLUMN sized INTEGER NOT NULL DEFAULT 0;
