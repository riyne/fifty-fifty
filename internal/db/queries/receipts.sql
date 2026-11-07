-- ConsumeReceiptQuota takes one unit of the user's quota for the current UTC day, or
-- returns no row when the day's limit (daily_limit) is already used. The conditional upsert is
-- the whole protection: the conflict path locks the row, so two uploads at limit-1
-- cannot both pass the WHERE. If the caller's transaction rolls back, so does the unit.
-- name: ConsumeReceiptQuota :one
INSERT INTO receipt_quota (user_id, day, used)
SELECT @user_id, (now() AT TIME ZONE 'UTC')::date, 1
WHERE @daily_limit::int > 0
ON CONFLICT (user_id, day) DO UPDATE SET used = receipt_quota.used + 1
WHERE receipt_quota.used < @daily_limit::int
RETURNING used;

-- CreateParsingExpense makes the expense an upload starts: no items, status
-- 'parsing'. Like CreateExpense it does no authorization of its own.
-- name: CreateParsingExpense :one
INSERT INTO expenses (group_id, payer_id, description, status)
VALUES ($1, $2, $3, 'parsing')
RETURNING id, group_id, payer_id, description, status, version, tax_minor, tip_minor,
          created_at, finalized_at, voided_at;

-- name: CreateReceipt :one
INSERT INTO receipts (expense_id, group_id, uploader_id, object_key, content_type, size_bytes, width_px, height_px)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING id, created_at;
