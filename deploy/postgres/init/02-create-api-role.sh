#!/bin/sh
# Runs once, when the postgres volume is first initialised. Creates the login
# role the API connects as. Its privileges are granted per database by
# migration 000003, which runs afterwards, so this script holds only the
# credential and the migrations hold the permissions.
#
# The postgres entrypoint may source this file rather than execute it, so it
# avoids `set -e`/`exit` and relies on psql's ON_ERROR_STOP, which the
# entrypoint treats as fatal.
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" \
    --set=api_password="${API_DB_PASSWORD:?API_DB_PASSWORD must be set}" <<'SQL'
CREATE ROLE trip_api LOGIN PASSWORD :'api_password';
SQL
