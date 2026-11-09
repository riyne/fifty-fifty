-- +goose Up

-- Periodic jobs: the worker's scheduler inserts "one job per period" for
-- recurring work such as idempotency-key cleanup. dedupe_key makes that safe when
-- several workers (or restarts) try at once: the key names the period, the unique
-- constraint lets exactly one insert through, and the rest do nothing. NULL for
-- ordinary jobs; a unique index ignores NULLs, so they are unaffected.
--
-- The key is unique across ALL states, not only live jobs, so a finished job for
-- period N still blocks a second job for period N; a new period has a new key.
ALTER TABLE jobs ADD COLUMN dedupe_key text UNIQUE CHECK (char_length(dedupe_key) BETWEEN 1 AND 200);

-- +goose Down
ALTER TABLE jobs DROP COLUMN dedupe_key;
