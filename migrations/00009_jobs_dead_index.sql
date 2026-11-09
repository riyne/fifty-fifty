-- +goose Up

-- The metrics endpoint reports how many jobs are dead-lettered, per kind,
-- on every scrape. Without this index that count would read every finished job in
-- the table (they are never pruned), so scrape cost would grow with history. With
-- it, together with jobs_claim_idx, the whole stats query touches only the jobs
-- that are not finished: 0.5 ms on 200k rows (200k succeeded, 50 dead, 2.5k live).
-- Dead jobs are few, so the index stays tiny.
CREATE INDEX jobs_dead_idx ON jobs (kind) WHERE state = 'dead';

-- +goose Down
DROP INDEX jobs_dead_idx;
