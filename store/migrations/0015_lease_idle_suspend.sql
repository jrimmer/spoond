-- Per-lease idle reclamation (2.5, #129 part 2): after this many seconds
-- without activity the sweep suspends this lease through the pause path
-- (memory continues; the next call resumes it). -1 means "the host
-- default" (IDLE_SUSPEND_DEFAULT_SECS, itself 0 = never), 0 means never,
-- >0 is seconds. Existing leases keep the host default.
ALTER TABLE leases ADD COLUMN idle_suspend INTEGER NOT NULL DEFAULT -1;
