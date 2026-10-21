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

-- name: ListGroupsForUser :many
SELECT g.id, g.name, g.currency, g.created_at, m.role
FROM groups g
JOIN memberships m ON m.group_id = g.id
WHERE m.user_id = $1
ORDER BY g.created_at DESC, g.id
LIMIT 100;

-- ListMembers has no authorization of its own. Call it only after
-- GetGroupForMember succeeded for the same group and caller.
-- name: ListMembers :many
SELECT u.id AS user_id, u.display_name, m.role, m.joined_at
FROM memberships m
JOIN users u ON u.id = m.user_id
WHERE m.group_id = $1
ORDER BY m.joined_at, u.id;
