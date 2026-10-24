-- Least-privilege role for the API process.
--
-- Migrations and API-key administration run as the schema owner. The API
-- connects as trip_api, which can read and write rows but cannot ALTER, DROP,
-- TRUNCATE or CREATE anything, so a bug or an injection in the API cannot
-- change or destroy the schema.
--
-- Roles are cluster-wide. In Docker Compose the postgres init script creates
-- trip_api with LOGIN and a password before migrations run. Elsewhere it is
-- created here as NOLOGIN if missing, and an operator enables login (ALTER
-- ROLE trip_api LOGIN PASSWORD ...) so that no credential lives in a migration.
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'trip_api') THEN
        CREATE ROLE trip_api NOLOGIN;
    END IF;
END
$$;

GRANT USAGE ON SCHEMA public TO trip_api;
GRANT SELECT, INSERT, UPDATE, DELETE ON trips, idempotency_keys TO trip_api;

-- Read-only: the API checks the schema version at startup.
GRANT SELECT ON schema_migrations TO trip_api;

-- Every later migration that adds a table grants trip_api exactly what it
-- needs, so privileges are reviewed together with the schema change.
