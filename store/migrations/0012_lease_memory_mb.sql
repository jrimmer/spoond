-- Per-lease memory charge (#128): the image's memory_mb stamped when
-- the lease was granted. Quota accounting sums this over a user's
-- running leases; caching it on the row keeps that sum off the image
-- catalog. Existing leases backfill from their image row; a lease
-- whose image row is gone stays 0 (uncharged), like a new lease of a
-- vanished image.
ALTER TABLE leases ADD COLUMN memory_mb INTEGER NOT NULL DEFAULT 0;
UPDATE leases SET memory_mb = COALESCE((
  SELECT i.memory_mb FROM images i WHERE i.name = leases.image
), 0);
