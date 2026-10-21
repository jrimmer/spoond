-- Lease-scoped self token (spoond #156): a guest may release or keep
-- alive only its own lease when the caller asked for that at create.
-- self_token says the lease has one; self_token_hash holds only the
-- sha256 hex of the token, never the token itself.
ALTER TABLE leases ADD COLUMN self_token INTEGER NOT NULL DEFAULT 0;
ALTER TABLE leases ADD COLUMN self_token_hash TEXT NOT NULL DEFAULT '';
