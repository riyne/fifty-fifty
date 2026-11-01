-- LockSettlement takes the settlement row FOR UPDATE and returns what Resolve needs
-- to decide. Every resolver (execute, reconcile, webhook) goes through this, so two
-- of them on one settlement run one after the other, and the second sees the first's
-- result.
-- name: LockSettlement :one
SELECT id, group_id, from_member, to_member, amount_minor, status
FROM settlements
WHERE id = $1
FOR UPDATE;

-- name: CreateSettlementTransaction :one
INSERT INTO ledger_transactions (group_id, kind, settlement_id, created_by)
VALUES (sqlc.arg(group_id), 'settlement', sqlc.arg(settlement_id)::uuid, sqlc.arg(created_by))
RETURNING id;

-- The Mark* statements repeat the allowed source statuses, so they can only ever move
-- a settlement forward even if the lock were somehow missing (the handler treats 0
-- rows as an error).

-- name: MarkSettlementSucceeded :execrows
UPDATE settlements
SET status = 'succeeded', ledger_transaction_id = sqlc.arg(ledger_transaction_id), resolved_at = now()
WHERE id = sqlc.arg(id) AND status IN ('pending', 'unknown');

-- name: MarkSettlementFailed :execrows
UPDATE settlements
SET status = 'failed', failure_code = sqlc.arg(failure_code), resolved_at = now()
WHERE id = sqlc.arg(id) AND status IN ('pending', 'unknown');

-- name: MarkSettlementUnknown :execrows
UPDATE settlements SET status = 'unknown'
WHERE id = $1 AND status = 'pending';

-- LockGroupForSettlement serializes settlement creation within a group. It is
-- FOR NO KEY UPDATE, not FOR UPDATE: every foreign-key insert into groups
-- (expenses, memberships, ledger rows) takes FOR KEY SHARE on this row, which
-- FOR UPDATE conflicts with and FOR NO KEY UPDATE does not. It conflicts only
-- with itself, so it stops two settlement creations from both reserving the same
-- money and nothing else (finalize and void do not take it).
-- name: LockGroupForSettlement :one
SELECT id FROM groups WHERE id = $1 FOR NO KEY UPDATE;

-- GetSettlementCapacity reads, in ONE statement (one snapshot), the four numbers
-- the availability rule needs: both members' ledger balances and the money already
-- reserved by their open (pending or unknown) settlements, as sender and as payee.
-- name: GetSettlementCapacity :one
SELECT
    COALESCE((SELECT SUM(le.amount_minor) FROM ledger_entries le
              WHERE le.group_id = sqlc.arg(group_id)::uuid AND le.member_id = sqlc.arg(from_member)::uuid), 0)::bigint AS from_balance,
    COALESCE((SELECT SUM(le.amount_minor) FROM ledger_entries le
              WHERE le.group_id = sqlc.arg(group_id)::uuid AND le.member_id = sqlc.arg(to_member)::uuid), 0)::bigint AS to_balance,
    COALESCE((SELECT SUM(s.amount_minor) FROM settlements s
              WHERE s.group_id = sqlc.arg(group_id)::uuid AND s.from_member = sqlc.arg(from_member)::uuid
                AND s.status IN ('pending', 'unknown')), 0)::bigint AS reserved_out,
    COALESCE((SELECT SUM(s.amount_minor) FROM settlements s
              WHERE s.group_id = sqlc.arg(group_id)::uuid AND s.to_member = sqlc.arg(to_member)::uuid
                AND s.status IN ('pending', 'unknown')), 0)::bigint AS reserved_in;

-- name: IsGroupMember :one
SELECT EXISTS (SELECT 1 FROM memberships WHERE group_id = $1 AND user_id = $2);

-- name: CreateSettlement :one
INSERT INTO settlements (group_id, from_member, to_member, amount_minor)
VALUES ($1, $2, $3, $4)
RETURNING id, group_id, from_member, to_member, amount_minor, status, failure_code, ledger_transaction_id, created_at, resolved_at;

-- GetSettlementForMember is the fetch and the authorization in one statement: the
-- caller must be a member of the path's group, and the settlement must be in it.
-- name: GetSettlementForMember :one
SELECT s.id, s.group_id, s.from_member, s.to_member, s.amount_minor, s.status, s.failure_code,
       s.ledger_transaction_id, s.created_at, s.resolved_at, g.currency
FROM settlements s
JOIN groups g ON g.id = s.group_id
JOIN memberships m ON m.group_id = s.group_id AND m.user_id = sqlc.arg(user_id)
WHERE s.id = sqlc.arg(settlement_id) AND s.group_id = sqlc.arg(group_id);

-- name: ListSettlementsForMember :many
SELECT s.id, s.group_id, s.from_member, s.to_member, s.amount_minor, s.status, s.failure_code,
       s.ledger_transaction_id, s.created_at, s.resolved_at, g.currency
FROM settlements s
JOIN groups g ON g.id = s.group_id
JOIN memberships m ON m.group_id = s.group_id AND m.user_id = sqlc.arg(user_id)
WHERE s.group_id = sqlc.arg(group_id)
ORDER BY s.created_at DESC, s.id
LIMIT 100;

-- GetSettlementForExecution is for the worker (settlement.execute and, later,
-- reconcile): it has no caller to authorize, it is given a job. It reads the current
-- state, so a re-run sees what an earlier run or a webhook already did.
-- name: GetSettlementForExecution :one
SELECT s.id, s.group_id, s.from_member, s.to_member, s.amount_minor, s.status, g.currency
FROM settlements s
JOIN groups g ON g.id = s.group_id
WHERE s.id = $1;
