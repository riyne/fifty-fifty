-- +goose Up

-- Expenses, items, claims and the double-entry ledger (DESIGN.md sections 4-5).
--
-- Design rule for this file: whatever the database can refuse, it refuses, so a
-- bug in a handler (wrong group, missing check) fails loudly instead of
-- corrupting money. The composite foreign keys below are what make a cross-group
-- row impossible, not just unlikely. What the database cannot enforce (it cannot
-- stop a claim racing a finalize) is handled by row locks in the application and
-- measured by the stress test.
--
-- All amounts are bigint minor units (cents). No floats.

-- ---------------------------------------------------------------------------
-- Expenses
-- ---------------------------------------------------------------------------

-- The creator is the payer (DESIGN.md M2 decisions). tax/tip are expense-level;
-- the total is derived (sum of items + tax + tip), never stored, so it cannot
-- drift from the items.
CREATE TABLE expenses (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    group_id     uuid        NOT NULL REFERENCES groups (id),
    payer_id     uuid        NOT NULL,
    description  text        NOT NULL CHECK (char_length(description) BETWEEN 1 AND 200),
    tax_minor    bigint      NOT NULL DEFAULT 0 CHECK (tax_minor >= 0),
    tip_minor    bigint      NOT NULL DEFAULT 0 CHECK (tip_minor >= 0),
    status       text        NOT NULL CHECK (status IN ('parsing', 'needs_review', 'claiming', 'finalized', 'void')),
    -- Optimistic lock for item edits; claims do not touch it (DESIGN.md section 5).
    version      integer     NOT NULL DEFAULT 1,
    created_at   timestamptz NOT NULL DEFAULT now(),
    finalized_at timestamptz,
    voided_at    timestamptz,
    -- Target of the composite foreign keys below, so child rows must agree on the group.
    UNIQUE (id, group_id),
    -- The payer must be a member of the expense's group.
    FOREIGN KEY (group_id, payer_id) REFERENCES memberships (group_id, user_id),
    -- A void expense was finalized first and keeps its finalized_at.
    CHECK ((finalized_at IS NOT NULL) = (status IN ('finalized', 'void'))),
    CHECK ((voided_at IS NOT NULL) = (status = 'void'))
);

CREATE INDEX expenses_group_idx ON expenses (group_id, created_at DESC, id);

-- One row per claimable unit (quantity lines are expanded before they get here).
CREATE TABLE expense_items (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    expense_id  uuid        NOT NULL REFERENCES expenses (id),
    position    integer     NOT NULL CHECK (position >= 0),
    description text        NOT NULL CHECK (char_length(description) BETWEEN 1 AND 200),
    price_minor bigint      NOT NULL CHECK (price_minor >= 0),
    UNIQUE (id, expense_id),
    UNIQUE (expense_id, position)
);

-- A claim says "I had this item". group_id and expense_id are denormalized so the
-- composite foreign keys can prove: the item belongs to that expense, the expense
-- belongs to that group, and the claimant is a member of that group. Claims are
-- the one mutable table here (unclaim deletes a row, until finalize).
CREATE TABLE claims (
    item_id    uuid        NOT NULL,
    expense_id uuid        NOT NULL,
    group_id   uuid        NOT NULL,
    member_id  uuid        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (item_id, member_id),
    FOREIGN KEY (item_id, expense_id) REFERENCES expense_items (id, expense_id),
    FOREIGN KEY (expense_id, group_id) REFERENCES expenses (id, group_id),
    FOREIGN KEY (group_id, member_id) REFERENCES memberships (group_id, user_id)
);

CREATE INDEX claims_expense_idx ON claims (expense_id);

-- ---------------------------------------------------------------------------
-- Ledger (append-only, double-entry)
-- ---------------------------------------------------------------------------

-- One row per posting. kind 'settlement' arrives with the settlements table in M4,
-- which will extend the kind check and add its link column.
--   expense: the finalize posting; exactly one per expense.
--   void:    reversal of that posting; at most one per expense.
CREATE TABLE ledger_transactions (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    group_id    uuid        NOT NULL REFERENCES groups (id),
    kind        text        NOT NULL CHECK (kind IN ('expense', 'void')),
    expense_id  uuid,
    reverses_id uuid,
    created_by  uuid        NOT NULL REFERENCES users (id),
    created_at  timestamptz NOT NULL DEFAULT now(),
    -- The database transaction that wrote this row. Entries may only be added by
    -- that same transaction (see ledger_entry_same_tx), so once it commits the
    -- posting is complete and balanced and nothing can be appended to it later.
    -- xid8 is 64-bit and does not wrap.
    txid        xid8        NOT NULL DEFAULT pg_current_xact_id(),
    UNIQUE (id, group_id),
    UNIQUE (id, group_id, expense_id),
    FOREIGN KEY (expense_id, group_id) REFERENCES expenses (id, group_id),
    -- A void reverses a posting of the same expense in the same group.
    FOREIGN KEY (reverses_id, group_id, expense_id) REFERENCES ledger_transactions (id, group_id, expense_id),
    CHECK (
        (kind = 'expense' AND expense_id IS NOT NULL AND reverses_id IS NULL) OR
        (kind = 'void'    AND expense_id IS NOT NULL AND reverses_id IS NOT NULL AND reverses_id <> id)
    )
);

-- At most one finalize posting and one void per expense. These are what make a
-- retried or duplicated finalize/void unable to post twice.
CREATE UNIQUE INDEX ledger_transactions_one_expense_posting
    ON ledger_transactions (expense_id) WHERE kind = 'expense';
CREATE UNIQUE INDEX ledger_transactions_one_void
    ON ledger_transactions (expense_id) WHERE kind = 'void';

-- Signed net amount per member per transaction: positive = the group owes them,
-- negative = they owe the group. Zero rows are not stored. A member's balance in
-- a group is SUM(amount_minor); balances across a group always sum to zero.
CREATE TABLE ledger_entries (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    transaction_id uuid   NOT NULL,
    group_id       uuid   NOT NULL,
    member_id      uuid   NOT NULL,
    amount_minor   bigint NOT NULL CHECK (amount_minor <> 0),
    UNIQUE (transaction_id, member_id),
    FOREIGN KEY (transaction_id, group_id) REFERENCES ledger_transactions (id, group_id),
    FOREIGN KEY (group_id, member_id) REFERENCES memberships (group_id, user_id)
);

-- Index-only scan for "balance per member in a group".
CREATE INDEX ledger_entries_balance_idx ON ledger_entries (group_id, member_id) INCLUDE (amount_minor);

-- +goose StatementBegin
-- Append-only: refuse UPDATE, DELETE and TRUNCATE on both ledger tables, for
-- everyone including the table owner and superusers. (The REVOKE below stops the
-- app role earlier, with a permission error; this trigger is the backstop. A
-- superuser can still disable triggers or change session_replication_role, so
-- this protects against bugs and a compromised app, not against the DBA.)
CREATE FUNCTION ledger_reject_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'ledger tables are append-only: % on % is not allowed', TG_OP, TG_TABLE_NAME
        USING ERRCODE = 'restrict_violation';
END
$$;
-- +goose StatementEnd

CREATE TRIGGER ledger_transactions_append_only
    BEFORE UPDATE OR DELETE ON ledger_transactions
    FOR EACH ROW EXECUTE FUNCTION ledger_reject_mutation();
CREATE TRIGGER ledger_transactions_no_truncate
    BEFORE TRUNCATE ON ledger_transactions
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_reject_mutation();
CREATE TRIGGER ledger_entries_append_only
    BEFORE UPDATE OR DELETE ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_reject_mutation();
CREATE TRIGGER ledger_entries_no_truncate
    BEFORE TRUNCATE ON ledger_entries
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_reject_mutation();

-- Defense in depth: the app role cannot even attempt it.
REVOKE UPDATE, DELETE ON ledger_transactions, ledger_entries FROM fifty_app;

-- +goose StatementBegin
-- Entries can only be inserted by the same database transaction that created
-- their header row. Without this, a later transaction could append a balanced
-- pair to an old posting: still zero-sum, but history rewritten.
CREATE FUNCTION ledger_entry_same_tx() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM ledger_transactions t
        WHERE t.id = NEW.transaction_id AND t.txid = pg_current_xact_id()
    ) THEN
        RAISE EXCEPTION 'ledger entries can only be added in the transaction that created ledger transaction %', NEW.transaction_id
            USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER ledger_entries_same_tx
    BEFORE INSERT ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_entry_same_tx();

-- +goose StatementBegin
-- Every ledger transaction sums to zero. Deferred, so it runs at COMMIT after
-- all of a posting's entries are in; it re-checks once per inserted entry, which
-- is cheap for the handful of entries a posting has. SUM(bigint) is numeric in
-- Postgres, so the check itself cannot overflow.
CREATE FUNCTION ledger_check_zero_sum() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    total numeric;
BEGIN
    SELECT COALESCE(SUM(amount_minor), 0) INTO total
    FROM ledger_entries WHERE transaction_id = NEW.transaction_id;
    IF total <> 0 THEN
        RAISE EXCEPTION 'ledger transaction % does not sum to zero (sum is %)', NEW.transaction_id, total
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NULL;
END
$$;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER ledger_entries_zero_sum
    AFTER INSERT ON ledger_entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION ledger_check_zero_sum();

-- +goose Down
DROP TABLE ledger_entries;
DROP TABLE ledger_transactions;
DROP FUNCTION ledger_check_zero_sum();
DROP FUNCTION ledger_entry_same_tx();
DROP FUNCTION ledger_reject_mutation();
DROP TABLE claims;
DROP TABLE expense_items;
DROP TABLE expenses;
