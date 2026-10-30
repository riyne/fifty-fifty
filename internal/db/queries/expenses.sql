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
