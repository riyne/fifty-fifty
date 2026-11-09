-- +goose Up

-- Users are keyed by the identity provider's (issuer, subject), never by email.
-- Rows are created on the first valid token (see internal/auth).
CREATE TABLE users (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    issuer       text        NOT NULL,
    subject      text        NOT NULL,
    email        text        NOT NULL DEFAULT '',
    display_name text        NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (issuer, subject)
);

-- One currency per group.
CREATE TABLE groups (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text        NOT NULL CHECK (char_length(name) BETWEEN 1 AND 100),
    currency   text        NOT NULL CHECK (currency IN ('CAD', 'USD')),
    created_by uuid        NOT NULL REFERENCES users (id),
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Membership is the authorization primitive: every group-scoped query joins
-- through this table.
CREATE TABLE memberships (
    group_id  uuid        NOT NULL REFERENCES groups (id),
    user_id   uuid        NOT NULL REFERENCES users (id),
    role      text        NOT NULL CHECK (role IN ('owner', 'member')),
    joined_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (group_id, user_id)
);

-- "Which groups am I in" lookups; the primary key covers the group_id side.
CREATE INDEX memberships_user_id_idx ON memberships (user_id);

-- +goose Down
DROP TABLE memberships;
DROP TABLE groups;
DROP TABLE users;
