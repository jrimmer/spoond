-- Preemption (#128 part 3): when a guaranteed admission cannot get
-- hugepages, spoond suspends burst leases through the pause path and
-- records preempted_at on each. The timestamp is the preemption instant;
-- it orders the resume queue (oldest preemption first) and is cleared
-- when the lease resumes. Empty = not preempted.
ALTER TABLE leases ADD COLUMN preempted_at TEXT NOT NULL DEFAULT '';
