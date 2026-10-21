-- name: CreateJoinCode :one
INSERT INTO join_codes (group_id, code_hash, created_by, max_uses, expires_at)
VALUES (
    sqlc.arg(group_id),
    sqlc.arg(code_hash),
    sqlc.arg(created_by),
    sqlc.arg(max_uses),
    now() + (sqlc.arg(ttl_hours)::int * interval '1 hour')
)
RETURNING id, max_uses, uses, expires_at, created_at;

-- Status is computed by the database so it uses the same clock as redemption.
-- name: ListJoinCodes :many
SELECT id, max_uses, uses, expires_at, revoked_at, created_at,
       (CASE
            WHEN revoked_at IS NOT NULL THEN 'revoked'
            WHEN expires_at <= now()    THEN 'expired'
            WHEN uses >= max_uses       THEN 'exhausted'
            ELSE 'active'
        END)::text AS status
FROM join_codes
WHERE group_id = $1
ORDER BY created_at DESC, id
LIMIT 100;

-- Scoped by group_id as well as id, so a code id from another group matches nothing.
-- name: RevokeJoinCode :execrows
UPDATE join_codes SET revoked_at = now()
WHERE id = $1 AND group_id = $2 AND revoked_at IS NULL;

-- name: JoinCodeExistsInGroup :one
SELECT EXISTS (SELECT 1 FROM join_codes WHERE id = $1 AND group_id = $2);

-- LockRedeemableJoinCode finds a usable code and row-locks it, which serializes
-- concurrent redeemers of the same code. Under READ COMMITTED, a waiter
-- re-evaluates the WHERE clause after the lock is released, so when two users
-- race for the last use the loser sees uses = max_uses and gets no row.
-- Not found, revoked, expired and exhausted are deliberately indistinguishable.
-- name: LockRedeemableJoinCode :one
SELECT id, group_id FROM join_codes
WHERE code_hash = $1
  AND revoked_at IS NULL
  AND expires_at > now()
  AND uses < max_uses
FOR UPDATE;

-- name: IncrementJoinCodeUses :exec
UPDATE join_codes SET uses = uses + 1 WHERE id = $1;

-- Returns rows affected: 1 if the user was added, 0 if already a member.
-- name: AddMembershipIfAbsent :execrows
INSERT INTO memberships (group_id, user_id, role)
VALUES ($1, $2, $3)
ON CONFLICT (group_id, user_id) DO NOTHING;

-- name: InsertAuditEvent :exec
INSERT INTO audit_events (actor_user_id, group_id, kind, data)
VALUES ($1, $2, $3, $4);
