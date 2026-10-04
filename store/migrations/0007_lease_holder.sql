-- What holds a lease (a CI job, an orchestrator's flight, a person's
-- scratch work) and an optional link to it. A lease with a non-empty
-- holder is left alone by the sweepers: not released at its TTL, not
-- idle-suspended, and checkpointed periodically like a persistent
-- lease. Existing leases get the empty holder: normal sweeping.
ALTER TABLE leases ADD COLUMN holder TEXT NOT NULL DEFAULT '';
ALTER TABLE leases ADD COLUMN holder_url TEXT NOT NULL DEFAULT '';
