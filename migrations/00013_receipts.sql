-- +goose Up

-- Receipt uploads (M6.6, DESIGN.md section 8). An upload creates an expense in status
-- 'parsing' and one receipts row that points at the stored image; the parse job (6.7)
-- turns it into items. The image itself is not in the database: object_key names a
-- file in the object store (a sha256 of the re-encoded bytes).
--
-- One receipt per expense. The composite foreign keys have the same job as in
-- migration 00005: a receipt cannot name an expense of another group, and its uploader
-- must be a member of the expense's group, whatever a handler does.
CREATE TABLE receipts (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    expense_id   uuid        NOT NULL UNIQUE,
    group_id     uuid        NOT NULL,
    uploader_id  uuid        NOT NULL,
    object_key   text        NOT NULL CHECK (object_key ~ '^[0-9a-f]{64}$'),
    content_type text        NOT NULL CHECK (content_type IN ('image/jpeg', 'image/png')),
    size_bytes   integer     NOT NULL CHECK (size_bytes > 0),
    width_px     integer     NOT NULL CHECK (width_px > 0),
    height_px    integer     NOT NULL CHECK (height_px > 0),
    created_at   timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (expense_id, group_id) REFERENCES expenses (id, group_id),
    FOREIGN KEY (group_id, uploader_id) REFERENCES memberships (group_id, user_id)
);

-- Daily upload quota per user: a cost cap (each receipt later costs an LLM call), not
-- a ledger protection. One row per user per UTC day; the application increments it
-- with a conditional upsert (see ConsumeReceiptQuota), so concurrent uploads cannot
-- take the counter past the limit. The limit is a parameter, not stored here.
CREATE TABLE receipt_quota (
    user_id uuid    NOT NULL REFERENCES users (id),
    day     date    NOT NULL,
    used    integer NOT NULL CHECK (used >= 0),
    PRIMARY KEY (user_id, day)
);

-- +goose Down
DROP TABLE receipt_quota;
DROP TABLE receipts;
