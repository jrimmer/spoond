-- Why a lease is lost (the detail of its lost event), so the owner
-- learns the cause from GET and from every 409 lease_lost response.
-- '' for a lease lost before this column existed; the API then says
-- the substrate lost it without naming a cause.
ALTER TABLE leases ADD COLUMN lost_reason TEXT NOT NULL DEFAULT '';
