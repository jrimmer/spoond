-- Per-lease sizes (#151 Z1): the lease owns its size. vcpu joins
-- memory_mb (0012) and disk_mb (0023) as the size the lease's VM runs
-- at, stamped from the build the VM was started from and kept through
-- pause, resume, restart, clone, fork, restore and recovery. vCPU is
-- never accounted; it is stored so a later cold start can boot the same
-- size.
--
-- The stamps become the truth first. Before 3.1, restore, recovery and
-- cold restart stamped the image's memory_mb, which differs from the VM
-- after an image was rebuilt at another size. A running lease's VM size
-- is on its sandboxes row; a paused lease's on its resume build. Then
-- whatever is still unknown is backfilled from the image catalog;
-- COALESCE keeps a lease whose image row is gone at 0 (unknown, read
-- as the image default later).
ALTER TABLE leases ADD COLUMN vcpu INTEGER NOT NULL DEFAULT 0;

UPDATE leases SET
	vcpu = (SELECT s.vcpu FROM sandboxes s WHERE s.sandbox_id = leases.sandbox_id),
	memory_mb = (SELECT s.memory_mb FROM sandboxes s WHERE s.sandbox_id = leases.sandbox_id)
WHERE suspended = 0 AND EXISTS (
	SELECT 1 FROM sandboxes s
	WHERE s.sandbox_id = leases.sandbox_id AND s.vcpu > 0 AND s.memory_mb > 0
);

UPDATE leases SET
	vcpu = (SELECT b.vcpu FROM builds b WHERE b.build_id = leases.resume_build_id),
	memory_mb = (SELECT b.memory_mb FROM builds b WHERE b.build_id = leases.resume_build_id)
WHERE suspended = 1 AND resume_build_id != '' AND EXISTS (
	SELECT 1 FROM builds b
	WHERE b.build_id = leases.resume_build_id AND b.vcpu > 0 AND b.memory_mb > 0
);

UPDATE leases SET vcpu = COALESCE((
	SELECT i.vcpu FROM images i WHERE i.name = leases.image
), 0)
WHERE vcpu = 0;
