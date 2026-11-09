-- +goose Up

-- Join codes are bearer secrets: only a SHA-256 of the normalized code is stored.
-- The plaintext is shown once at creation and cannot be recovered.
CREATE TABLE join_codes (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    group_id   uuid        NOT NULL REFERENCES groups (id),
    code_hash  bytea       NOT NULL UNIQUE,
    created_by uuid        NOT NULL REFERENCES users (id),
    max_uses   integer     NOT NULL CHECK (max_uses BETWEEN 1 AND 50),
    uses       integer     NOT NULL DEFAULT 0,
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK (uses >= 0 AND uses <= max_uses)
);

CREATE INDEX join_codes_group_id_idx ON join_codes (group_id);

-- Scoped per user. A row is inserted (as a placeholder) in the same transaction
-- as the operation it protects and completed before commit, so a committed row
-- always has a stored response and a crash leaves no trace. See internal/idempotency.
CREATE TABLE idempotency_keys (
    user_id         uuid        NOT NULL REFERENCES users (id),
    key             text        NOT NULL CHECK (char_length(key) BETWEEN 1 AND 255),
    fingerprint     bytea       NOT NULL,
    response_status integer     NOT NULL,
    response_body   bytea       NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    expires_at      timestamptz NOT NULL,
    PRIMARY KEY (user_id, key)
);

-- For the cleanup job (added with the queue).
CREATE INDEX idempotency_keys_expires_at_idx ON idempotency_keys (expires_at);

-- Append-only record of authorization-relevant actions. Never put secrets
-- (join code plaintext, tokens) in data.
CREATE TABLE audit_events (
    id            bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    actor_user_id uuid        NOT NULL REFERENCES users (id),
    group_id      uuid        NOT NULL REFERENCES groups (id),
    kind          text        NOT NULL,
    data          jsonb       NOT NULL DEFAULT '{}',
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX audit_events_group_id_idx ON audit_events (group_id, id);

-- +goose Down
DROP TABLE audit_events;
DROP TABLE idempotency_keys;
DROP TABLE join_codes;
