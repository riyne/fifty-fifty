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
