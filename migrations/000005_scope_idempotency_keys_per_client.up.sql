-- Idempotency keys become per client: two clients can never collide on, or
-- read each other's results through, the same key.
--
-- Rows created before authentication belong to no client. They are only retry
-- state and expire within IDEMPOTENCY_TTL anyway, so they are discarded.
DELETE FROM idempotency_keys;

ALTER TABLE idempotency_keys
    ADD COLUMN client_id UUID NOT NULL REFERENCES api_clients (id) ON DELETE CASCADE;

-- client_id leads the key, so it also serves the ON DELETE CASCADE lookup.
ALTER TABLE idempotency_keys DROP CONSTRAINT idempotency_keys_pkey;
ALTER TABLE idempotency_keys
    ADD CONSTRAINT idempotency_keys_pkey PRIMARY KEY (client_id, scope, idempotency_key);
