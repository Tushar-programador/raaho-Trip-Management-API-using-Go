-- Keys from different clients may share a value, which the restored primary
-- key would reject, so the (ephemeral) retry state is discarded.
DELETE FROM idempotency_keys;

ALTER TABLE idempotency_keys DROP CONSTRAINT idempotency_keys_pkey;
ALTER TABLE idempotency_keys DROP COLUMN client_id;
ALTER TABLE idempotency_keys
    ADD CONSTRAINT idempotency_keys_pkey PRIMARY KEY (scope, idempotency_key);
