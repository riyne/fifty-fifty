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

-- GetReceiptForParse is what the parse job reads first: the receipt, where its image is,
-- and whether its expense is still waiting. It does no authorization: the job's payload
-- is an id the api wrote itself.
-- name: GetReceiptForParse :one
SELECT r.id, r.expense_id, r.group_id, r.uploader_id, r.object_key, r.content_type,
       r.parsed_at, e.status AS expense_status
FROM receipts r
JOIN expenses e ON e.id = r.expense_id
WHERE r.id = $1;

-- MarkExpenseParsed moves an expense out of `parsing`. It is conditional on the status,
-- so the parse job and the sweep cannot both do it: whoever comes second gets no row and
-- must write nothing. The version goes up because the items and totals change, so an edit
-- made from an earlier read conflicts.
-- name: MarkExpenseParsed :one
UPDATE expenses
SET status = @status::text, version = version + 1, tax_minor = @tax_minor, tip_minor = @tip_minor
WHERE id = @id AND status = 'parsing'
RETURNING version;

-- RecordReceiptParse writes the run's facts on the receipt, once.
-- name: RecordReceiptParse :execrows
UPDATE receipts
SET parsed_at = now(), parse_outcome = @outcome::text, parse_reasons = @reasons::text[],
    ocr_chars = @ocr_chars, redacted_cards = @redacted_cards, redacted_emails = @redacted_emails,
    redacted_phones = @redacted_phones, model = @model, input_tokens = @input_tokens,
    output_tokens = @output_tokens, printed_total_minor = @printed_total_minor, diff_minor = @diff_minor
WHERE id = @id AND parsed_at IS NULL;

-- ListStuckReceipts finds receipts whose expense is still `parsing` and that no live parse
-- job (queued or running) will ever finish: the job died (dead-lettered) or was never
-- written. It locks the expense rows it returns and skips rows another sweep holds, so
-- overlapping sweeps take different receipts. The kind is a literal so the partial index
-- on jobs applies.
-- name: ListStuckReceipts :many
SELECT r.id, r.expense_id, r.group_id, r.uploader_id
FROM receipts r
JOIN expenses e ON e.id = r.expense_id
WHERE r.parsed_at IS NULL
  AND e.status = 'parsing'
  AND r.created_at < now() - make_interval(secs => @stuck_secs::float8)
  AND (@group_id::uuid = '00000000-0000-0000-0000-000000000000' OR r.group_id = @group_id::uuid)
  AND NOT EXISTS (
      SELECT 1 FROM jobs j
      WHERE j.kind = 'receipt.parse' AND j.payload->>'receipt_id' = r.id::text AND j.state IN ('queued', 'running'))
ORDER BY r.created_at
LIMIT @batch::int
FOR UPDATE OF e SKIP LOCKED;
