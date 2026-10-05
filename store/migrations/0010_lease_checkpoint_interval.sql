-- Per-lease periodic checkpoint interval (2.3, #122): how often the
-- host checkpoints this lease, in seconds. -1 means "the host default"
-- (CHECKPOINT_INTERVAL_MINS, itself 0 = never), 0 means never, >0 is
-- seconds. Existing leases keep the host default.
ALTER TABLE leases ADD COLUMN checkpoint_interval INTEGER NOT NULL DEFAULT -1;
