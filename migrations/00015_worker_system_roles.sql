-- +goose Up

-- Two more database roles, for the code that runs without a user. Row-level
-- security (migration 00016) limits fifty_app, the role of the user-request path, to the
-- caller's groups. The processes below act for no user, so each gets its own role
-- with explicit policies, and with grants limited to the tables its statements
-- touch. Neither role is a superuser, an owner or BYPASSRLS.
--
--   fifty_worker  the worker processes (cmd/worker): the job queue, settlement
--                 execution, reconcile and the watchdog, receipt parsing and its
--                 sweep, and the idempotency-key cleanup.
--   fifty_system  the api's second pool: first-login provisioning in the auth
--                 middleware (users) and the payment webhook (settlements and the
--                 ledger). It runs inside the api process, so a bug there is a bug
--                 in a process that also serves users: the narrower grants matter.
--
-- The grants below were derived from the statements each process runs and checked
-- by running the real processes as these roles (test/e2e) and by test/dbinvariants.
-- Tables created by later migrations are NOT granted to either role automatically
-- (only fifty_app has default privileges); a migration that needs it must say so.
--
-- The passwords are the local-development throwaway, as for fifty_app (00004). A
-- real deployment must set them out of band. Roles are cluster-wide.

-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'fifty_worker') THEN
        CREATE ROLE fifty_worker LOGIN PASSWORD 'fifty'
            NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'fifty_system') THEN
        CREATE ROLE fifty_system LOGIN PASSWORD 'fifty'
            NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS;
    END IF;
END
$$;
-- +goose StatementEnd

GRANT USAGE ON SCHEMA public TO fifty_worker, fifty_system;

-- fifty_worker --------------------------------------------------------------

-- The queue owns this table outright.
GRANT SELECT, INSERT, UPDATE, DELETE ON jobs TO fifty_worker;
-- Settlement execution, reconcile, the watchdog and Resolve. Settlements are
-- created by the api, never by the worker. groups is read for the currency.
GRANT SELECT, UPDATE ON settlements TO fifty_worker;
GRANT SELECT ON groups TO fifty_worker;
-- Resolve posts the settlement. Ledger rows are append-only for every role
-- (00005); the worker can read and add, nothing else.
GRANT SELECT, INSERT ON ledger_transactions, ledger_entries TO fifty_worker;
GRANT INSERT ON audit_events TO fifty_worker;
-- Receipt parsing: read the receipt and its expense, write the items and mark the
-- expense and the receipt parsed; the sweep moves stuck expenses to review.
GRANT SELECT, UPDATE ON expenses, receipts TO fifty_worker;
GRANT SELECT, INSERT ON expense_items TO fifty_worker;
-- The hourly cleanup of expired keys.
GRANT SELECT, DELETE ON idempotency_keys TO fifty_worker;

-- fifty_system --------------------------------------------------------------

-- First login: look the user up, create it if absent. Nothing else about users.
GRANT SELECT, INSERT ON users TO fifty_system;
-- EnsureUser is INSERT ... ON CONFLICT DO UPDATE SET issuer = EXCLUDED.issuer (a no-op
-- update so RETURNING yields the existing row); Postgres wants UPDATE on that column.
GRANT UPDATE (issuer) ON users TO fifty_system;
-- The payment webhook resolves a settlement through settlements.Resolve.
GRANT SELECT, UPDATE ON settlements TO fifty_system;
GRANT SELECT ON groups TO fifty_system;
GRANT SELECT, INSERT ON ledger_transactions, ledger_entries TO fifty_system;
GRANT INSERT ON audit_events TO fifty_system;

-- +goose Down
-- Drops the roles' privileges in this database. Roles are cluster-wide, so this
-- fails if another database in the cluster still grants them anything (as 00004).
DROP OWNED BY fifty_worker;
DROP OWNED BY fifty_system;
DROP ROLE fifty_worker;
DROP ROLE fifty_system;
