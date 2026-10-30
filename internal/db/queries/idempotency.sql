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

-- DeleteExpiredIdempotencyKeys removes up to `batch` expired keys. Expired keys
-- are already ignored by every reader; this only reclaims the space.
--
-- The outer `expires_at <= now()` looks redundant next to the subquery's and is
-- not. ClaimIdempotencyKey takes over an expired row by UPDATEing it back to
-- live. If that happens between this statement choosing the row and deleting it,
-- the DELETE waits for the claim's transaction and then sees the NEW version of
-- the row. Postgres re-checks the DELETE's own conditions on that version, but
-- not the subquery's, so without the outer condition it would delete a key that
-- a request in flight has just re-claimed. (The subquery's own condition is
-- equivalent once the outer one exists, since ORDER BY puts expired keys first; it
-- stays so the subquery says what it selects.)
-- name: DeleteExpiredIdempotencyKeys :execrows
DELETE FROM idempotency_keys
WHERE expires_at <= now()
  AND (user_id, key) IN (
      SELECT user_id, key
      FROM idempotency_keys
      WHERE expires_at <= now()
      ORDER BY expires_at
      LIMIT sqlc.arg('batch')
  );
