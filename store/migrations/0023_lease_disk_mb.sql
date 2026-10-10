-- The running disk reservation (FS3a, owner decision 2026-10-08):
-- disk_mb is the lease's whole disk allowance in MiB, from its image's
-- disk_mb, stamped when the guest is granted (beside memory_mb). Summed
-- over the RUNNING leases it is the snapshot disk's running reservation
-- the disk room accounting keeps free: bytes a VM already wrote count
-- again, which is accepted as the conservative option.
--
-- Backfill from the image catalog: every live lease re-stamps on its
-- next grant or restart, so a missing number only ages out. COALESCE
-- keeps a lease whose image row is gone at 0 (unknown) rather than
-- NULL.
ALTER TABLE leases ADD COLUMN disk_mb INTEGER NOT NULL DEFAULT 0;

UPDATE leases SET disk_mb = COALESCE((
	SELECT i.disk_mb FROM images i WHERE i.name = leases.image
), 0)
WHERE disk_mb = 0;
