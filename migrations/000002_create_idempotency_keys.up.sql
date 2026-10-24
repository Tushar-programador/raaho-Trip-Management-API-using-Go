-- Idempotency keys for POST /trips.
--
-- The (scope, idempotency_key) primary key is what makes concurrent requests
-- with the same key safe: the key row is inserted in the same transaction that
-- creates the trip, so a second request blocks on the unique index until the
-- first commits (then replays its result) or rolls back (then proceeds itself).
--
-- trip_snapshot holds the trip exactly as it was returned to the first caller,
-- so a replay returns the original response even if the trip later changed or
-- was deleted. trip_id deliberately has no foreign key for the same reason.
CREATE TABLE idempotency_keys (
    scope            VARCHAR(64)  NOT NULL,
    idempotency_key  VARCHAR(255) NOT NULL,
    request_hash     CHAR(64)     NOT NULL,
    trip_id          UUID,
    trip_snapshot    JSONB,
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT now(),
    expires_at       TIMESTAMPTZ  NOT NULL,

    CONSTRAINT idempotency_keys_pkey PRIMARY KEY (scope, idempotency_key),
    CONSTRAINT idempotency_keys_key_not_blank CHECK (btrim(idempotency_key) <> ''),
    CONSTRAINT idempotency_keys_expires_after_created CHECK (expires_at > created_at),
    -- Either both result columns are set (completed) or neither is. Within this
    -- service a row is only ever visible to others in the completed state.
    CONSTRAINT idempotency_keys_result_complete CHECK (
        (trip_id IS NULL) = (trip_snapshot IS NULL)
    )
);

-- Supports purging expired keys (DELETE ... WHERE expires_at < now()).
CREATE INDEX idempotency_keys_expires_at_idx ON idempotency_keys (expires_at);
