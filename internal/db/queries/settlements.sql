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
