-- +goose Up

-- 00006 gave the claim two partial indexes (due queued jobs by run_at, running jobs
-- by lease_expires_at). Measured on 200k rows (100k finished, 50k due, 50k
-- delayed) the claim over those two with `OR` could not return rows in
-- order: the planner bitmap-ORed both, read every due row and sorted them, 11.9 ms
-- per claim and growing with the backlog. A claim wants "the oldest few eligible
-- jobs", which needs ONE ordered index.
--
-- So eligibility becomes one timestamp, as in SQS's visibility timeout: while a
-- job is running, run_at is its lease expiry. A job is claimable when
-- run_at <= now(), whether it is a queued job that is due or a running job whose
-- lease has run out. The same claim then does an ordered index scan that stops
-- after the first few rows (0.03 ms on the same data).
--
-- Cost, accepted: each heartbeat now changes an indexed column (run_at), so it
-- writes a new index entry instead of a HOT update. At one heartbeat per running
-- job every lease/3 seconds that is negligible here.
--
-- lease_expires_at stays as its own column: it reads clearly in code and queries
-- and is NULL unless the job is running, which the 00006 CHECKs rely on. The
-- CHECK below keeps the two from drifting.
DROP INDEX jobs_queued_idx;
DROP INDEX jobs_lease_idx;

ALTER TABLE jobs ADD CONSTRAINT jobs_run_at_is_lease_expiry
    CHECK (state <> 'running' OR run_at = lease_expires_at);

-- Holds only claimable rows. Running jobs with a live lease are in it too, but
-- sort after nothing-due and are bounded by the number of in-flight jobs.
CREATE INDEX jobs_claim_idx ON jobs (run_at) WHERE state IN ('queued', 'running');

-- +goose Down
DROP INDEX jobs_claim_idx;
ALTER TABLE jobs DROP CONSTRAINT jobs_run_at_is_lease_expiry;
CREATE INDEX jobs_queued_idx ON jobs (run_at) WHERE state = 'queued';
CREATE INDEX jobs_lease_idx ON jobs (lease_expires_at) WHERE state = 'running';
