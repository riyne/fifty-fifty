-- GetGroupBalances returns every current member of the group with their net
-- balance: the SUM of their ledger entries in that group (0 if they have none).
-- Positive means the group owes them, negative means they owe the group.
--
-- It is one statement, so all balances come from the same snapshot and always sum
-- to zero. The join on the caller's own membership (me) means a non-member gets no
-- rows; the handler also checks membership first so it can answer 404.
--
-- It also returns, in the same statement and so the same snapshot, the money each member has
-- tied up in OPEN settlements (pending or unknown: asked of the payment provider, not yet in
-- the ledger): what they are sending and what they are to receive. Those are the numbers
-- create-settlement subtracts (GetSettlementCapacity), so the suggested transfers can be
-- limited to what it will accept.
-- name: GetGroupBalances :many
SELECT m.user_id AS member_id, u.display_name, COALESCE(b.balance, 0)::bigint AS balance_minor,
       COALESCE(ro.total, 0)::bigint AS reserved_out_minor,
       COALESCE(ri.total, 0)::bigint AS reserved_in_minor
FROM memberships m
JOIN users u ON u.id = m.user_id
JOIN memberships me ON me.group_id = m.group_id AND me.user_id = sqlc.arg(caller_id)
LEFT JOIN (
    SELECT le.member_id, SUM(le.amount_minor) AS balance
    FROM ledger_entries le
    WHERE le.group_id = sqlc.arg(group_id)
    GROUP BY le.member_id
) b ON b.member_id = m.user_id
LEFT JOIN (
    SELECT s.from_member, SUM(s.amount_minor) AS total
    FROM settlements s
    WHERE s.group_id = sqlc.arg(group_id) AND s.status IN ('pending', 'unknown')
    GROUP BY s.from_member
) ro ON ro.from_member = m.user_id
LEFT JOIN (
    SELECT s.to_member, SUM(s.amount_minor) AS total
    FROM settlements s
    WHERE s.group_id = sqlc.arg(group_id) AND s.status IN ('pending', 'unknown')
    GROUP BY s.to_member
) ri ON ri.to_member = m.user_id
WHERE m.group_id = sqlc.arg(group_id)
ORDER BY m.joined_at, m.user_id;
