-- name: CreateGroup :one
INSERT INTO groups (name, currency, created_by)
VALUES ($1, $2, $3)
RETURNING *;

-- name: AddMembership :exec
INSERT INTO memberships (group_id, user_id, role)
VALUES ($1, $2, $3);

-- GetGroupForMember is both the fetch and the authorization check: a caller who
-- is not a member gets no rows, indistinguishable from a group that does not exist.
-- name: GetGroupForMember :one
SELECT g.id, g.name, g.currency, g.created_at, m.role
FROM groups g
JOIN memberships m ON m.group_id = g.id
WHERE g.id = $1 AND m.user_id = $2;

-- One page, newest first. Keyset pagination: after_created_at and after_id are the last
-- row of the previous page (both NULL for the first page); page_size is one more than the
-- page the caller wants, so that it can tell whether another page exists. The cursor only
-- moves the position inside rows the membership join already allows.
-- name: ListGroupsForUser :many
SELECT g.id, g.name, g.currency, g.created_at, m.role
FROM groups g
JOIN memberships m ON m.group_id = g.id
WHERE m.user_id = sqlc.arg(user_id)
  AND (sqlc.narg(after_id)::uuid IS NULL
       OR g.created_at < sqlc.narg(after_created_at)::timestamptz
       OR (g.created_at = sqlc.narg(after_created_at)::timestamptz AND g.id > sqlc.narg(after_id)::uuid))
ORDER BY g.created_at DESC, g.id
LIMIT sqlc.arg(page_size)::int;

-- ListMembers has no authorization of its own. Call it only after
-- GetGroupForMember succeeded for the same group and caller.
-- name: ListMembers :many
SELECT u.id AS user_id, u.display_name, m.role, m.joined_at
FROM memberships m
JOIN users u ON u.id = m.user_id
WHERE m.group_id = $1
ORDER BY m.joined_at, u.id;
