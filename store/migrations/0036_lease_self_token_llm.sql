-- LLM access for a lease's self token (spoond #162): when a create asks
-- for self_token_llm, the guest may present its lease-scoped self token
-- at the LLM gateway instead of its owner's LLM key. Off by default, so a
-- self token alone never grants LLM access.
ALTER TABLE leases ADD COLUMN self_token_llm INTEGER NOT NULL DEFAULT 0;
