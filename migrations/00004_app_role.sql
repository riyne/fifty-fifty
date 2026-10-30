-- +goose Up

-- Until now the api and the tests connected as the compose superuser `fifty`,
-- which owns every table and bypasses GRANT/REVOKE entirely. That makes "the app
-- cannot UPDATE or DELETE the ledger" impossible to enforce, so the app gets its
-- own role. Migrations keep running as the owner (`fifty`).
--
-- fifty_app: login, not superuser, not owner, no DDL. The password below is the
-- local-development throwaway (same as the compose credentials). A real
-- deployment must run `ALTER ROLE fifty_app PASSWORD '...'` out of band; never
-- reuse this value.

-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'fifty_app') THEN
        CREATE ROLE fifty_app LOGIN PASSWORD 'fifty'
            NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS;
    END IF;
END
$$;
-- +goose StatementEnd

GRANT USAGE ON SCHEMA public TO fifty_app;

GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO fifty_app;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO fifty_app;

-- Tables and sequences created by later migrations (run as the owner) get the
-- same grants automatically. Migrations that need less (the ledger) REVOKE.
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO fifty_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT USAGE, SELECT ON SEQUENCES TO fifty_app;

-- The migration tool's bookkeeping table is not the app's business.
REVOKE ALL ON goose_db_version FROM fifty_app;
REVOKE ALL ON SEQUENCE goose_db_version_id_seq FROM fifty_app;

-- Written once, never changed (see the comment in 00003). Nothing in the app
-- updates or deletes these rows.
REVOKE UPDATE, DELETE ON audit_events FROM fifty_app;

-- +goose Down
DROP OWNED BY fifty_app;
DROP ROLE fifty_app;
