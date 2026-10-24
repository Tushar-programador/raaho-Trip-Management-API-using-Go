-- API clients authenticate with a bearer key.
--
-- Only a SHA-256 hash of each key is stored. Keys are 256-bit random values,
-- so a fast hash is safe (there is nothing to brute-force, unlike passwords),
-- and a leak of this table does not reveal usable keys. key_prefix holds the
-- first characters so a key can be identified in listings and logs.
CREATE TABLE api_clients (
    id          UUID         PRIMARY KEY,
    name        VARCHAR(100) NOT NULL,
    key_prefix  VARCHAR(16)  NOT NULL,
    key_hash    CHAR(64)     NOT NULL,
    created_at  TIMESTAMPTZ  NOT NULL DEFAULT now(),
    revoked_at  TIMESTAMPTZ,

    -- Also the index for the per-request lookup by key hash.
    CONSTRAINT api_clients_key_hash_key UNIQUE (key_hash),
    CONSTRAINT api_clients_name_not_blank CHECK (btrim(name) <> ''),
    CONSTRAINT api_clients_revoked_after_created CHECK (revoked_at IS NULL OR revoked_at >= created_at)
);

-- The API only authenticates clients. Creating and revoking keys is an
-- administrative operation performed as the owner (cmd/apikey).
GRANT SELECT ON api_clients TO trip_api;
