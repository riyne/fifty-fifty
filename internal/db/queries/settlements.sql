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
  AND (sqlc.narg(after_id)::uuid IS NULL
       OR s.created_at < sqlc.narg(after_created_at)::timestamptz
       OR (s.created_at = sqlc.narg(after_created_at)::timestamptz AND s.id > sqlc.narg(after_id)::uuid))
ORDER BY s.created_at DESC, s.id
LIMIT sqlc.arg(page_size)::int;

-- GetSettlementForExecution is for the worker (settlement.execute and, later,
-- reconcile): it has no caller to authorize, it is given a job. It reads the current
-- state, so a re-run sees what an earlier run or a webhook already did.
-- name: GetSettlementForExecution :one
SELECT s.id, s.group_id, s.from_member, s.to_member, s.amount_minor, s.status, g.currency
FROM settlements s
JOIN groups g ON g.id = s.group_id
WHERE s.id = $1;

-- LockSettlementStatus is how a handler's commit finds out, under the row lock,
-- where a settlement is NOW (a webhook may have moved it since the handler read it).
-- name: LockSettlementStatus :one
SELECT status FROM settlements WHERE id = $1 FOR UPDATE;

-- ListStuckSettlements finds pending settlements whose execute job has been lost: no
-- execute job is queued or running for them, none was created within the last
-- stuck_secs seconds (so a job that just died is not replaced at once), and fewer than
-- max_jobs have ever been created (so a settlement the provider keeps rejecting
-- cannot loop forever). Oldest first. FOR UPDATE SKIP LOCKED makes two sweeps that
-- overlap take different rows instead of both enqueueing for the same one.
--
-- SKIP LOCKED alone is NOT enough, and this is easy to get wrong. The statement's
-- snapshot is taken when it starts. If a concurrent sweep commits its new job after
-- that, but before this statement reaches the row, the row's lock is free and its
-- NOT EXISTS was evaluated without the new job, so this statement returns it anyway:
-- Postgres re-checks the WHERE clause after a lock wait only for rows that were
-- UPDATED, and the other sweep only locked the settlement. The caller therefore
-- re-asks IsSettlementStillStuck for each row in a new statement, which sees every job
-- committed so far, and no job for a settlement can be committed while it holds the lock.
--
-- The kind is a literal on purpose: jobs_settlement_idx is partial, and a parameter
-- would give a cached generic plan that cannot use it.
-- name: ListStuckSettlements :many
SELECT s.id, s.group_id, s.from_member,
       (SELECT count(*) FROM jobs j
         WHERE j.kind = 'settlement.execute' AND j.payload->>'settlement_id' = s.id::text)::int AS execute_jobs
FROM settlements s
WHERE s.status = 'pending'
  AND (sqlc.narg(group_id)::uuid IS NULL OR s.group_id = sqlc.narg(group_id)::uuid)
  AND s.created_at < now() - make_interval(secs => sqlc.arg(stuck_secs)::float8)
  AND NOT EXISTS (
      SELECT 1 FROM jobs j
      WHERE j.kind = 'settlement.execute' AND j.payload->>'settlement_id' = s.id::text
        AND (j.state IN ('queued', 'running') OR j.created_at > now() - make_interval(secs => sqlc.arg(stuck_secs)::float8)))
  AND (SELECT count(*) FROM jobs j
        WHERE j.kind = 'settlement.execute' AND j.payload->>'settlement_id' = s.id::text) < sqlc.arg(max_jobs)::int
ORDER BY s.created_at, s.id
LIMIT sqlc.arg(batch)::int
FOR UPDATE OF s SKIP LOCKED;

-- IsSettlementStillStuck repeats ListStuckSettlements' conditions for one settlement
-- that the caller already holds the row lock on, in a statement of its own. Keep the
-- two in step.
-- name: IsSettlementStillStuck :one
SELECT EXISTS (
    SELECT 1 FROM settlements s
    WHERE s.id = sqlc.arg(id)::uuid
      AND s.status = 'pending'
      AND NOT EXISTS (
          SELECT 1 FROM jobs j
          WHERE j.kind = 'settlement.execute' AND j.payload->>'settlement_id' = s.id::text
            AND (j.state IN ('queued', 'running') OR j.created_at > now() - make_interval(secs => sqlc.arg(stuck_secs)::float8)))
      AND (SELECT count(*) FROM jobs j
            WHERE j.kind = 'settlement.execute' AND j.payload->>'settlement_id' = s.id::text) < sqlc.arg(max_jobs)::int
);
