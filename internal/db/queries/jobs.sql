-- run_at defaults to the database's clock, not the caller's, so enqueueing "now"
-- is ordered the same as every other timestamp the queue compares.
-- name: InsertJob :one
INSERT INTO jobs (kind, payload, max_attempts, run_at)
VALUES (
    sqlc.arg('kind'),
    sqlc.arg('payload'),
    sqlc.arg('max_attempts'),
    COALESCE(sqlc.narg('run_at')::timestamptz, now())
)
RETURNING *;

-- ClaimJobs hands up to `batch` eligible jobs of the given kinds to a worker.
--
-- Eligible means: queued, or running with an expired lease (while running, run_at
-- is the lease expiry, see migration 00007), and run_at has passed, and the job
-- has an attempt left. FOR UPDATE SKIP LOCKED makes concurrent claimers pass over
-- rows another claimer is holding instead of waiting for them, so two workers
-- never get the same job and never queue behind each other.
--
-- MATERIALIZED states that the candidate selection (with its LIMIT and row locks)
-- runs exactly once. Postgres 17 already refuses to inline a CTE that takes row
-- locks (checked with EXPLAIN, and removing the keyword changes no plan), so this
-- is redundant today; it is spelled out so the claim does not depend on that
-- planner behavior. If a planner folded the CTE into the UPDATE's join and applied
-- the LIMIT per candidate row, a claim could take more than `batch` jobs.
--
-- Claiming writes a NEW lease_token every time. Every later write by a worker is
-- conditional on that token, so a worker whose lease was taken over cannot affect
-- the newer attempt (fencing). last_error is kept, so a retried job still shows
-- why the previous attempt failed.
--
-- lease_seconds is applied to the database clock, never the worker's.
-- name: ClaimJobs :many
WITH candidates AS MATERIALIZED (
    SELECT id
    FROM jobs
    WHERE state IN ('queued', 'running')
      AND run_at <= now()
      AND attempts < max_attempts
      AND kind = ANY(sqlc.arg('kinds')::text[])
    ORDER BY run_at
    LIMIT sqlc.arg('batch')
    FOR UPDATE SKIP LOCKED
)
UPDATE jobs
SET state            = 'running',
    attempts         = jobs.attempts + 1,
    lease_token      = gen_random_uuid(),
    lease_expires_at = now() + make_interval(secs => sqlc.arg('lease_seconds')::float8),
    run_at           = now() + make_interval(secs => sqlc.arg('lease_seconds')::float8)
FROM candidates
WHERE jobs.id = candidates.id
RETURNING jobs.*;

-- ReapExhaustedJobs dead-letters running jobs whose lease has expired on their
-- last attempt. ClaimJobs refuses them (attempts = max_attempts, and the table
-- forbids attempts > max_attempts), so without this they would stay "running"
-- forever. run_at is the lease expiry while running.
-- name: ReapExhaustedJobs :many
UPDATE jobs
SET state            = 'dead',
    finished_at      = now(),
    lease_token      = NULL,
    lease_expires_at = NULL,
    last_error       = 'lease expired on attempt ' || attempts || ' of ' || max_attempts
WHERE state = 'running'
  AND run_at <= now()
  AND attempts >= max_attempts
RETURNING id;

-- The three statements below are the worker's only writes to a job it holds, and
-- each is conditional on the lease token: that is the fence. A worker whose lease
-- was taken over (ClaimJobs writes a new token on every claim) matches no row and
-- changes nothing, however late it arrives. Zero rows means "you no longer hold
-- this job"; the Go side turns that into ErrLeaseLost.
--
-- There is deliberately no `state = 'running'` here. The table's CHECK makes
-- lease_token non-null exactly while a job is running (migration 00006), so a
-- finished or requeued job has a NULL token, `NULL = x` is never true, and a
-- matching token already implies running. A mutation check showed no test could
-- tell the extra condition apart. If that CHECK is ever relaxed, these three
-- statements need the state condition back.

-- Extends the lease. run_at moves with it (see migration 00007). Succeeds on a
-- lease that has expired but has not been taken over yet: the token still
-- matches and nobody else holds the job, so keeping it is harmless.
-- name: HeartbeatJob :one
UPDATE jobs
SET lease_expires_at = now() + make_interval(secs => sqlc.arg('lease_seconds')::float8),
    run_at           = now() + make_interval(secs => sqlc.arg('lease_seconds')::float8)
WHERE id = sqlc.arg('id')
  AND lease_token = sqlc.arg('lease_token')::uuid
RETURNING lease_expires_at;

-- name: CompleteJob :execrows
UPDATE jobs
SET state            = 'succeeded',
    finished_at      = now(),
    lease_token      = NULL,
    lease_expires_at = NULL
WHERE id = sqlc.arg('id')
  AND lease_token = sqlc.arg('lease_token')::uuid;

-- Gives a failed attempt back: queued to run again at retry_at (default now), or
-- dead when this was the last attempt. attempts is not touched; the next claim
-- counts the next attempt. Returns the state the job ended in.
-- name: FailJob :one
UPDATE jobs
SET state            = CASE WHEN attempts >= max_attempts THEN 'dead' ELSE 'queued' END,
    finished_at      = CASE WHEN attempts >= max_attempts THEN now() END,
    run_at           = CASE WHEN attempts >= max_attempts THEN run_at
                            ELSE COALESCE(sqlc.narg('retry_at')::timestamptz, now()) END,
    lease_token      = NULL,
    lease_expires_at = NULL,
    last_error       = sqlc.arg('last_error')::text
WHERE id = sqlc.arg('id')
  AND lease_token = sqlc.arg('lease_token')::uuid
RETURNING state;
