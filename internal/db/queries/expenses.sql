-- CreateExpense does no authorization of its own. Call it only after
-- GetGroupForMember succeeded for the same group and caller. The foreign key on
-- (group_id, payer_id) is the backstop: a non-member payer cannot be inserted.
-- name: CreateExpense :one
INSERT INTO expenses (group_id, payer_id, description, tax_minor, tip_minor, status)
VALUES ($1, $2, $3, $4, $5, 'claiming')
RETURNING id, group_id, payer_id, description, status, version, tax_minor, tip_minor,
          created_at, finalized_at, voided_at;

-- CreateExpenseItems inserts all items of one expense in a single statement. The
-- three unnest calls in one SELECT list advance together (Postgres 10+), so they
-- act as one zipped table; the arrays must have equal length.
-- name: CreateExpenseItems :many
INSERT INTO expense_items (expense_id, position, description, price_minor)
SELECT sqlc.arg(expense_id)::uuid,
       unnest(sqlc.arg(positions)::int[]),
       unnest(sqlc.arg(descriptions)::text[]),
       unnest(sqlc.arg(prices)::bigint[])
RETURNING id, position, description, price_minor;

-- GetExpenseForMember is both the fetch and the authorization check: the join on
-- memberships (for the caller) and the filter on the path's group mean a caller
-- who is not in that group, an expense in some other group, and an id that does
-- not exist all produce no rows.
-- name: GetExpenseForMember :one
SELECT e.id, e.group_id, e.payer_id, e.description, e.status, e.version,
       e.tax_minor, e.tip_minor, e.created_at, e.finalized_at, e.voided_at,
       g.currency,
       COALESCE((SELECT SUM(i.price_minor) FROM expense_items i WHERE i.expense_id = e.id), 0)::bigint AS subtotal_minor
FROM expenses e
JOIN groups g ON g.id = e.group_id
JOIN memberships m ON m.group_id = e.group_id AND m.user_id = sqlc.arg(user_id)
WHERE e.id = sqlc.arg(expense_id) AND e.group_id = sqlc.arg(group_id);

-- ListExpenseItems has no authorization of its own. Call it only after
-- GetExpenseForMember succeeded for the same expense.
-- name: ListExpenseItems :many
SELECT i.id, i.position, i.description, i.price_minor,
       COALESCE(array_agg(c.member_id ORDER BY c.member_id) FILTER (WHERE c.member_id IS NOT NULL), '{}')::uuid[] AS claimed_by
FROM expense_items i
LEFT JOIN claims c ON c.item_id = i.id
WHERE i.expense_id = $1
GROUP BY i.id
ORDER BY i.position;

-- ListExpensesForMember: same membership join as GetExpenseForMember, so a
-- non-member gets an empty list from the query itself.
-- name: ListExpensesForMember :many
SELECT e.id, e.payer_id, e.description, e.status, e.tax_minor, e.tip_minor,
       e.created_at, g.currency,
       COALESCE(s.subtotal, 0)::bigint AS subtotal_minor,
       COALESCE(s.item_count, 0)::int AS item_count
FROM expenses e
JOIN groups g ON g.id = e.group_id
JOIN memberships m ON m.group_id = e.group_id AND m.user_id = sqlc.arg(user_id)
LEFT JOIN LATERAL (
    SELECT SUM(i.price_minor) AS subtotal, COUNT(*) AS item_count
    FROM expense_items i WHERE i.expense_id = e.id
) s ON true
WHERE e.group_id = sqlc.arg(group_id)
ORDER BY e.created_at DESC, e.id
LIMIT 100;

-- LockExpenseForClaim is the first statement of every claim and unclaim: it
-- takes FOR SHARE on the expense row, authorizes the caller (membership join,
-- path-group filter) and returns the status, all in one statement.
--
-- FOR SHARE locks are compatible with each other, so claimers never wait for
-- one another. They conflict with FOR UPDATE, which finalize, void and item
-- edits take: those wait for in-flight claims, and a claim that arrives while
-- one of them holds the lock waits, then (READ COMMITTED) re-reads the row after
-- it commits and sees the new status.
-- name: LockExpenseForClaim :one
SELECT e.status
FROM expenses e
JOIN memberships m ON m.group_id = e.group_id AND m.user_id = sqlc.arg(user_id)
WHERE e.id = sqlc.arg(expense_id) AND e.group_id = sqlc.arg(group_id)
FOR SHARE OF e;

-- GetExpenseItem has no authorization of its own. Call it only after
-- LockExpenseForClaim succeeded for the same expense. The expense filter is what
-- makes an item id from some other expense match nothing.
-- name: GetExpenseItem :one
SELECT i.id, i.position, i.description, i.price_minor,
       COALESCE(array_agg(c.member_id ORDER BY c.member_id) FILTER (WHERE c.member_id IS NOT NULL), '{}')::uuid[] AS claimed_by
FROM expense_items i
LEFT JOIN claims c ON c.item_id = i.id
WHERE i.id = $1 AND i.expense_id = $2
GROUP BY i.id;

-- name: AddClaim :execrows
INSERT INTO claims (item_id, expense_id, group_id, member_id)
VALUES ($1, $2, $3, $4)
ON CONFLICT (item_id, member_id) DO NOTHING;

-- RemoveClaim only ever removes the caller's own claim.
-- name: RemoveClaim :execrows
DELETE FROM claims WHERE item_id = $1 AND expense_id = $2 AND member_id = $3;

-- LockExpenseForFinalize takes FOR UPDATE on the expense row, which waits for every
-- in-flight claim (FOR SHARE) and blocks new ones until commit. It authorizes in
-- the same statement: the caller must be the expense's payer, in the path's group,
-- otherwise no row. There is no membership join because none is needed: the
-- foreign key on expenses (group_id, payer_id) guarantees a payer is a member of
-- the expense's group. The handler checks the payer earlier, without a lock, so a
-- non-payer never takes this write lock; this is the backstop.
-- name: LockExpenseForFinalize :one
SELECT e.status, e.tax_minor, e.tip_minor
FROM expenses e
WHERE e.id = sqlc.arg(expense_id) AND e.group_id = sqlc.arg(group_id) AND e.payer_id = sqlc.arg(user_id)
FOR UPDATE OF e;

-- name: CountGroupMembers :one
SELECT count(*) FROM memberships WHERE group_id = sqlc.arg(group_id) AND user_id = ANY(sqlc.arg(member_ids)::uuid[]);

-- AddClaimsForItems writes one claim per (item, member): "split the rest evenly"
-- becomes real claim rows, so the ledger can always be recomputed from claims.
-- name: AddClaimsForItems :exec
INSERT INTO claims (item_id, expense_id, group_id, member_id)
SELECT i.id, sqlc.arg(expense_id)::uuid, sqlc.arg(group_id)::uuid, m.id
FROM unnest(sqlc.arg(item_ids)::uuid[]) AS i(id)
CROSS JOIN unnest(sqlc.arg(member_ids)::uuid[]) AS m(id);

-- name: CreateLedgerTransaction :one
INSERT INTO ledger_transactions (group_id, kind, expense_id, created_by)
VALUES (sqlc.arg(group_id), 'expense', sqlc.arg(expense_id)::uuid, sqlc.arg(created_by))
RETURNING id;

-- CreateLedgerEntries inserts every entry of one posting in a single statement
-- (the unnest calls advance together). The posting must be in the same database
-- transaction as its header, and must sum to zero, or the database refuses it.
-- name: CreateLedgerEntries :exec
INSERT INTO ledger_entries (transaction_id, group_id, member_id, amount_minor)
SELECT sqlc.arg(transaction_id)::uuid,
       sqlc.arg(group_id)::uuid,
       unnest(sqlc.arg(member_ids)::uuid[]),
       unnest(sqlc.arg(amounts)::bigint[]);

-- MarkExpenseFinalized is conditional on the status, so it can only ever move a
-- claiming expense forward, even if the lock were somehow missing.
-- name: MarkExpenseFinalized :execrows
UPDATE expenses SET status = 'finalized', finalized_at = now()
WHERE id = $1 AND status = 'claiming';
