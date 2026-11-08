-- Dex keeps its state (clients' sessions, codes, refresh tokens, signing keys) in its own
-- database and role in the same Postgres, never in the application's `fifty` database.
-- Idempotent: run by `make up` (psql inside the container) and by the CI workflow (psql on the
-- runner). Development credentials, as everywhere in deploy/. Roles are cluster-wide; this one
-- owns only the `dex` database and has no privilege on `fifty`'s tables.
SELECT 'CREATE ROLE dex LOGIN PASSWORD ''dex''' WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'dex')\gexec
SELECT 'CREATE DATABASE dex OWNER dex' WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'dex')\gexec
