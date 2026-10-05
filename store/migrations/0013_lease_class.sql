-- Lease class and scheduling priority (#128 part 2): "guaranteed" while
-- the owner's running charge (this lease included) stays within their
-- guaranteed_mib, "burst" above it or when the request forced burst; a
-- burst lease must also leave the node's burst reserve free after its
-- own hugepages. Priority orders preemption within a class (lower is
-- preempted first, 0 the default). Existing leases are guaranteed,
-- which keeps their behaviour unchanged.
ALTER TABLE leases ADD COLUMN class TEXT NOT NULL DEFAULT 'guaranteed';
ALTER TABLE leases ADD COLUMN priority INTEGER NOT NULL DEFAULT 0;
