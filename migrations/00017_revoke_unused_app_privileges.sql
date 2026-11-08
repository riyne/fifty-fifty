-- +goose Up

-- fifty_app, the role of the user-request path, was given SELECT, INSERT, UPDATE and DELETE
-- on every table (00004), minus the ledger and the audit log (00005). Row-level security
-- (00016) limits which ROWS it can reach, but its policies are mostly `ALL` policies, so
-- they do not limit which COMMANDS it can run; only the grants do. This migration takes
-- away every privilege that no statement of the user-request path uses, so that a bug or
-- an injected statement in that path cannot do what the code never does (delete a group,
-- rewrite a settlement, delete a job).
--
-- How the list was derived (M10.16): from the sqlc queries and the Go packages that call
-- them (the user path is groups, expenses, receiptapi, balances and the settlement
-- handler, with idempotency and the job enqueue), checked by running the whole test
-- suite against a database where fifty_app had SELECT only and reading the "permission
-- denied" errors in the server log, then against the result of this migration.
--
-- Two kinds of UPDATE are easy to miss, and both stay:
--   * SELECT ... FOR UPDATE / FOR NO KEY UPDATE / FOR SHARE needs the UPDATE privilege on
--     the table. expenses (the claim, finalize, void and edit locks) and groups (the
--     settlement group lock) keep UPDATE for that, although no statement updates a
--     group. The row lock IS the use.
--   * INSERT ... ON CONFLICT DO UPDATE needs UPDATE: idempotency_keys (an expired key is
--     reclaimed) and receipt_quota (the daily count).
--
-- What fifty_app has afterwards (S select, I insert, U update, D delete); the test
-- test/dbinvariants/roles_test.go spells out the same table:
--   audit_events SI   claims SID   expense_items SIUD   expenses SIU   groups SIU
--   idempotency_keys SIU   jobs SI   join_codes SIU   ledger_entries SI
--   ledger_transactions SI   memberships SI   receipt_quota SIU   receipts SI
--   settlements SI   users S
-- Everything else about those tables (a settlement's status, a job's state, a receipt's
-- parse result, a membership's role) is written only by fifty_worker and fifty_system
-- (00015) or by the SECURITY DEFINER functions of 00016.

REVOKE DELETE ON expenses, groups, idempotency_keys, join_codes, receipt_quota FROM fifty_app;
REVOKE UPDATE, DELETE ON jobs, memberships, receipts, settlements FROM fifty_app;
REVOKE UPDATE ON claims FROM fifty_app;
REVOKE INSERT, UPDATE, DELETE ON users FROM fifty_app;

-- A table added by a later migration used to be granted to fifty_app automatically (00004),
-- which is how every table ended up with all four privileges. From now on a migration
-- that adds a table says what fifty_app may do with it, as for the other two roles (00015).
ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE SELECT, INSERT, UPDATE, DELETE ON TABLES FROM fifty_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE USAGE, SELECT ON SEQUENCES FROM fifty_app;

-- Every database lets every role create temporary tables by default (PUBLIC has TEMPORARY).
-- Nothing in the product creates one (the benchmark that does runs as the owner, a
-- superuser), so the privilege is removed from PUBLIC in this database.
-- +goose StatementBegin
DO $$
BEGIN
    EXECUTE format('REVOKE TEMPORARY ON DATABASE %I FROM PUBLIC', current_database());
END
$$;
-- +goose StatementEnd

-- +goose Down
-- Puts back exactly what the Up removed: the state 00004 and 00005 left.
-- +goose StatementBegin
DO $$
BEGIN
    EXECUTE format('GRANT TEMPORARY ON DATABASE %I TO PUBLIC', current_database());
END
$$;
-- +goose StatementEnd

ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO fifty_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO fifty_app;

GRANT INSERT, UPDATE, DELETE ON users TO fifty_app;
GRANT UPDATE ON claims TO fifty_app;
GRANT UPDATE, DELETE ON jobs, memberships, receipts, settlements TO fifty_app;
GRANT DELETE ON expenses, groups, idempotency_keys, join_codes, receipt_quota TO fifty_app;
