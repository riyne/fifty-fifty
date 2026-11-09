-- +goose Up

-- The job queue (docs/ARCHITECTURE.md section 5). One table; workers claim rows with
-- FOR UPDATE SKIP LOCKED. This migration is only the shape, and the shape
-- is where the lease rules live: whatever the database can refuse, it refuses,
-- so a bug in the claim or completion code cannot leave a job in a state that
-- means nothing (running with no lease, finished with no finish time).
--
-- State machine (transitions are enforced by the queue code, the combinations of
-- columns by the CHECKs below):
--
--   queued    --claim-->     running   (attempts + 1, fresh lease_token, lease_expires_at set)
--   running   --heartbeat--> running   (lease_expires_at extended; token must match)
--   running   --complete-->  succeeded (token must match)
--   running   --fail-->      queued    (run_at = backoff) or dead when attempts = max_attempts
--   running   --lease expired--> claimable again by the same claim statement
--   dead      --operator requeue--> queued
--
-- lease_token is the fencing token: it is regenerated on every claim, and every
-- write by a worker is conditional on it, so a worker that lost its lease (slow,
-- paused, partitioned) cannot overwrite the attempt that replaced it.
CREATE TABLE jobs (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    kind             text        NOT NULL CHECK (char_length(kind) BETWEEN 1 AND 100),
    payload          jsonb       NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(payload) = 'object'),
    state            text        NOT NULL DEFAULT 'queued' CHECK (state IN ('queued', 'running', 'succeeded', 'dead')),
    -- Number of claims so far. A claim increments it, so while a job runs it is
    -- the number of the current attempt.
    attempts         integer     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    max_attempts     integer     NOT NULL CHECK (max_attempts >= 1),
    -- When a queued job becomes eligible; moved forward by backoff on failure.
    run_at           timestamptz NOT NULL DEFAULT now(),
    lease_token      uuid,
    lease_expires_at timestamptz,
    last_error       text,
    created_at       timestamptz NOT NULL DEFAULT now(),
    finished_at      timestamptz,
    -- A job never runs more than max_attempts times. The claim statement must
    -- therefore not claim an expired lease on a job that has used its last
    -- attempt; it is moved to dead instead.
    CHECK (attempts <= max_attempts),
    -- Lease columns exist exactly while the job is running.
    CHECK ((state = 'running') = (lease_token IS NOT NULL)),
    CHECK ((state = 'running') = (lease_expires_at IS NOT NULL)),
    -- finished_at exists exactly when the job is in a terminal state.
    CHECK ((state IN ('succeeded', 'dead')) = (finished_at IS NOT NULL))
);

-- The two sets a claim looks at: due queued jobs, and running jobs whose lease
-- has run out. Partial, so the index only holds rows that can still be claimed
-- and stays small however many finished jobs pile up. EXPLAIN shows
-- that the claim statement uses them.
CREATE INDEX jobs_queued_idx ON jobs (run_at) WHERE state = 'queued';
CREATE INDEX jobs_lease_idx ON jobs (lease_expires_at) WHERE state = 'running';

-- +goose Down
DROP TABLE jobs;
