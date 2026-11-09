-- +goose Up

-- Pruning deletes succeeded jobs older than the retention period, oldest
-- first, in batches. Without this index every batch would scan the whole jobs table
-- (the pruner exists because that table otherwise only grows). The index holds only
-- succeeded jobs and every success adds one entry to it, a small write cost on the
-- completion path that the throughput benchmark includes. Measured once on 400k rows (200k past retention), not as a benchmark: one batch of 1000 took 1.7 ms with the index and 56.8 ms without it (sequential scan). Dead jobs are not pruned
-- (an operator may still want to requeue them) and live jobs have no finished_at.
CREATE INDEX jobs_finished_idx ON jobs (finished_at) WHERE state = 'succeeded';

-- +goose Down
DROP INDEX jobs_finished_idx;
