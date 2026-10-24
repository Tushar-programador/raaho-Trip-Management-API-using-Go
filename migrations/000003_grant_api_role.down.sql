REVOKE SELECT ON schema_migrations FROM trip_api;
REVOKE SELECT, INSERT, UPDATE, DELETE ON trips, idempotency_keys FROM trip_api;
REVOKE USAGE ON SCHEMA public FROM trip_api;
-- The role itself is cluster-wide and may be used by other databases, so it is
-- not dropped here.
