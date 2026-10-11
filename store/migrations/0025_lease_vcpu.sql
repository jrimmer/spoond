-- Per-lease sizes (#151 Z1): the lease owns its size. vcpu joins
-- memory_mb (0012) and disk_mb (0023) as the size the lease's VM runs
-- at, stamped from the build the VM was started from and kept through
-- pause, resume, restart, clone, fork, restore and recovery. vCPU is
-- never accounted; it is stored so a later cold start can boot the same
-- size.
--
-- Backfill from the image catalog, the size every existing lease was
-- granted at; COALESCE keeps a lease whose image row is gone at 0
-- (unknown, read as the image default later).
ALTER TABLE leases ADD COLUMN vcpu INTEGER NOT NULL DEFAULT 0;

UPDATE leases SET vcpu = COALESCE((
	SELECT i.vcpu FROM images i WHERE i.name = leases.image
), 0)
WHERE vcpu = 0;
