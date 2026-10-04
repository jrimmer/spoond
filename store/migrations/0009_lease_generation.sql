-- Which continuity generation a lease is on (2.2, #112): the count of
-- times the guest's memory did not continue from where its processes
-- left it (crash recovery, restart). A planned suspend/resume and the
-- admin drain/undrain continue the memory and keep the number. The
-- generation is returned by the lease API and written into the guest at
-- /run/spoond/generation. Existing leases start at 1, the value every
-- fresh lease is created with.
ALTER TABLE leases ADD COLUMN generation INTEGER NOT NULL DEFAULT 1;
