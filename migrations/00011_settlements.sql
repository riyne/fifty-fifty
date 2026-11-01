-- +goose Up

-- Settlements (DESIGN.md section 7, decision log "M4 plan decisions"): a member
-- records that they paid another member through the (simulated) payment provider.
-- Same rule as 00005: whatever the database can refuse, it refuses.
--
-- The sender is the member who owes (their balance rises when it succeeds). The
-- sender is also the creator, so there is no separate created_by.
--
-- Status machine, enforced in Go by settlements.Transition and here by the CHECKs:
--   pending -> succeeded | failed | unknown;  unknown -> succeeded | failed.
-- succeeded and failed are terminal. The CHECKs cannot see the previous status, so
-- "no transition out of a terminal state" is application code; what they do make
-- impossible is a succeeded settlement without its ledger posting.
CREATE TABLE settlements (
    id                    uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    group_id              uuid        NOT NULL REFERENCES groups (id),
    from_member           uuid        NOT NULL,
    to_member             uuid        NOT NULL,
    amount_minor          bigint      NOT NULL CHECK (amount_minor > 0),
    status                text        NOT NULL DEFAULT 'pending'
                                      CHECK (status IN ('pending', 'succeeded', 'failed', 'unknown')),
    -- The provider's decline reason, set exactly when failed.
    failure_code          text        CHECK (char_length(failure_code) BETWEEN 1 AND 100),
    -- The ledger posting, set exactly when succeeded (see the foreign key below).
    ledger_transaction_id uuid,
    created_at            timestamptz NOT NULL DEFAULT now(),
    -- When it reached a terminal state.
    resolved_at           timestamptz,
    -- Target of the composite foreign key from ledger_transactions.
    UNIQUE (id, group_id),
    FOREIGN KEY (group_id, from_member) REFERENCES memberships (group_id, user_id),
    FOREIGN KEY (group_id, to_member)   REFERENCES memberships (group_id, user_id),
    CHECK (from_member <> to_member),
    CHECK ((status = 'failed')                       = (failure_code IS NOT NULL)),
    CHECK ((status = 'succeeded')                    = (ledger_transaction_id IS NOT NULL)),
    CHECK ((status IN ('succeeded', 'failed'))       = (resolved_at IS NOT NULL))
);

-- Group listing, newest first.
CREATE INDEX settlements_group_idx ON settlements (group_id, created_at DESC, id);
-- Money that may still move: what create-settlement reserves against (4.2) and what
-- the watchdog scans (4.5). Small, because settlements leave it when they resolve.
CREATE INDEX settlements_open_idx ON settlements (group_id) WHERE status IN ('pending', 'unknown');

-- The ledger learns a third kind of posting. The old CHECKs are unnamed, so they
-- have Postgres' default names; the shape check is rewritten with a new name.
ALTER TABLE ledger_transactions
    DROP CONSTRAINT ledger_transactions_kind_check,
    DROP CONSTRAINT ledger_transactions_check;

ALTER TABLE ledger_transactions ADD COLUMN settlement_id uuid;

ALTER TABLE ledger_transactions
    ADD CONSTRAINT ledger_transactions_kind_check
        CHECK (kind IN ('expense', 'void', 'settlement')),
    ADD CONSTRAINT ledger_transactions_shape_check CHECK (
        (kind = 'expense'    AND expense_id IS NOT NULL AND reverses_id IS NULL     AND settlement_id IS NULL) OR
        (kind = 'void'       AND expense_id IS NOT NULL AND reverses_id IS NOT NULL AND reverses_id <> id AND settlement_id IS NULL) OR
        (kind = 'settlement' AND expense_id IS NULL     AND reverses_id IS NULL     AND settlement_id IS NOT NULL)
    ),
    -- A settlement posting is in its settlement's group.
    ADD FOREIGN KEY (settlement_id, group_id) REFERENCES settlements (id, group_id),
    -- Target of the settlement's link back to its own posting.
    ADD UNIQUE (id, settlement_id);

-- At most one posting per settlement: this is what makes a duplicated success
-- (retry, webhook, reconcile) unable to post twice even if the row lock in
-- settlements.Resolve were missing.
CREATE UNIQUE INDEX ledger_transactions_one_settlement_posting
    ON ledger_transactions (settlement_id) WHERE kind = 'settlement';

-- A succeeded settlement points at ITS OWN posting, not any posting in the group:
-- the pair (ledger_transaction_id, id) must exist as (id, settlement_id) on the
-- ledger side. Foreign keys are skipped when a column is null, so this is checked
-- exactly when ledger_transaction_id is set, i.e. when the status is succeeded.
-- The group agrees because the posting's own foreign key ties it to the settlement's group.
ALTER TABLE settlements
    ADD FOREIGN KEY (ledger_transaction_id, id) REFERENCES ledger_transactions (id, settlement_id);

-- +goose Down
-- Fails if a settlement has been posted: ledger rows cannot be deleted, so there is
-- nothing to restore the old kind check against. That is the intended behavior.
-- Dropping the column also drops the constraints that mention it (the shape check,
-- the foreign key, the unique constraint, the index) and, with CASCADE, the
-- settlements foreign key that depends on the unique constraint.
ALTER TABLE ledger_transactions DROP COLUMN settlement_id CASCADE;
DROP TABLE settlements;
ALTER TABLE ledger_transactions DROP CONSTRAINT ledger_transactions_kind_check;
ALTER TABLE ledger_transactions
    ADD CONSTRAINT ledger_transactions_kind_check CHECK (kind IN ('expense', 'void')),
    ADD CONSTRAINT ledger_transactions_check CHECK (
        (kind = 'expense' AND expense_id IS NOT NULL AND reverses_id IS NULL) OR
        (kind = 'void'    AND expense_id IS NOT NULL AND reverses_id IS NOT NULL AND reverses_id <> id)
    );
