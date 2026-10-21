-- ClaimIdempotencyKey inserts a placeholder row. If a live (unexpired) row
-- already exists the conflict clause's WHERE is false, nothing is returned, and
-- the caller reads the existing row. If the existing row's inserting transaction
-- is still open, this statement blocks on the unique index until it commits or
-- rolls back; that wait is what makes a concurrent duplicate replay the first
-- request's result instead of running twice. Expired rows are taken over.
-- name: ClaimIdempotencyKey :one
INSERT INTO idempotency_keys (user_id, key, fingerprint, response_status, response_body, expires_at)
VALUES ($1, $2, $3, 0, ''::bytea, now() + interval '24 hours')
ON CONFLICT (user_id, key) DO UPDATE
SET fingerprint     = EXCLUDED.fingerprint,
    response_status = 0,
    response_body   = ''::bytea,
    created_at      = now(),
    expires_at      = EXCLUDED.expires_at
WHERE idempotency_keys.expires_at <= now()
RETURNING user_id;

-- name: GetLiveIdempotencyKey :one
SELECT fingerprint, response_status, response_body
FROM idempotency_keys
WHERE user_id = $1 AND key = $2 AND expires_at > now();

-- name: CompleteIdempotencyKey :exec
UPDATE idempotency_keys
SET response_status = $3, response_body = $4
WHERE user_id = $1 AND key = $2;
