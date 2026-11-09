-- +goose Up

-- The settlement watchdog asks, for each stuck settlement, which execute jobs
-- exist for it and how recent they are; the reconcile handler enqueues its own next
-- check. Both look jobs up by the settlement id in the payload, and without this index
-- each lookup scans the whole jobs table (which only shrinks when finished jobs are
-- pruned, a week later). The index is partial, so it holds only settlement jobs and
-- adds nothing to the write path of any other kind. Queries must name the kind as a
-- literal for the planner to see that the predicate holds.
CREATE INDEX jobs_settlement_idx ON jobs ((payload->>'settlement_id'), kind)
    WHERE kind IN ('settlement.execute', 'settlement.reconcile');

-- +goose Down
DROP INDEX jobs_settlement_idx;
