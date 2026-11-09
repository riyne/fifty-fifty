-- Baseline no-op. Exists so the migration tooling can be exercised end to end
-- before any schema is designed. The next migration adds the first real tables (users,
-- groups, memberships); this file can stay as migration 1.

-- +goose Up
SELECT 1;

-- +goose Down
SELECT 1;
