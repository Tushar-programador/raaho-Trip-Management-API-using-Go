-- Runs once, when the postgres volume is first initialised. The integration
-- tests truncate tables, so they get their own database.
CREATE DATABASE trips_test;
