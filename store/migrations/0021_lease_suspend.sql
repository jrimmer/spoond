-- Structured suspension facts (#145 D6): why an automatic suspend happened,
-- the pressure policy step that ordered it (empty until the pressure order
-- names steps), the pause build it wrote and when. Empty/zero for a lease
-- suspended before the columns existed and for a hand or drain suspend,
-- which carry no automatic reason. Cleared on resume.
ALTER TABLE leases ADD COLUMN suspend_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE leases ADD COLUMN suspend_policy_step TEXT NOT NULL DEFAULT '';
ALTER TABLE leases ADD COLUMN suspend_build_id TEXT NOT NULL DEFAULT '';
ALTER TABLE leases ADD COLUMN suspended_at TEXT NOT NULL DEFAULT '';
