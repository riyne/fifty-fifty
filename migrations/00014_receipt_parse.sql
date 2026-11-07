-- +goose Up

-- What the parse job (M6.7) records about a receipt. Everything here is about the run,
-- never the content: counts, token usage, the reasons a person has to look. The OCR text
-- and the model's answer are not stored.
--
-- parsed_at and parse_outcome are set together, once, by whoever moves the expense out of
-- `parsing`: the parse handler, or the sweep that gives up on a receipt whose job died.
-- A receipt with parsed_at NULL is still waiting.
ALTER TABLE receipts
    ADD COLUMN parsed_at           timestamptz,
    ADD COLUMN parse_outcome       text    CHECK (parse_outcome IN ('claiming', 'needs_review')),
    -- Why a person has to look: the validator's reasons (invalid_output, too_many_units,
    -- total_mismatch, free_items_with_tax_or_tip) and the pipeline's own.
    ADD COLUMN parse_reasons       text[]  NOT NULL DEFAULT '{}'
        CHECK (parse_reasons <@ ARRAY['invalid_output', 'too_many_units', 'total_mismatch', 'free_items_with_tax_or_tip',
                                      'no_text', 'image_unavailable', 'ocr_failed', 'extractor_failed', 'parse_abandoned']::text[]),
    -- Counts are NULL when the run never got that far.
    ADD COLUMN ocr_chars           integer CHECK (ocr_chars >= 0),
    ADD COLUMN redacted_cards      integer CHECK (redacted_cards >= 0),
    ADD COLUMN redacted_emails     integer CHECK (redacted_emails >= 0),
    ADD COLUMN redacted_phones     integer CHECK (redacted_phones >= 0),
    ADD COLUMN model               text    CHECK (char_length(model) <= 100),
    ADD COLUMN input_tokens        integer CHECK (input_tokens >= 0),
    ADD COLUMN output_tokens       integer CHECK (output_tokens >= 0),
    ADD COLUMN printed_total_minor bigint  CHECK (printed_total_minor >= 0),
    ADD COLUMN diff_minor          bigint,
    ADD CONSTRAINT receipts_parse_shape CHECK ((parsed_at IS NULL) = (parse_outcome IS NULL));

-- The sweep looks for receipts still waiting, oldest first.
CREATE INDEX receipts_unparsed_idx ON receipts (created_at) WHERE parsed_at IS NULL;

-- The sweep asks which parse jobs exist for a receipt; without this each lookup scans the
-- whole jobs table (see jobs_settlement_idx). Queries must name the kind as a literal.
CREATE INDEX jobs_receipt_idx ON jobs ((payload->>'receipt_id')) WHERE kind = 'receipt.parse';

-- +goose Down
DROP INDEX jobs_receipt_idx;
DROP INDEX receipts_unparsed_idx;
ALTER TABLE receipts
    DROP CONSTRAINT receipts_parse_shape,
    DROP COLUMN diff_minor, DROP COLUMN printed_total_minor, DROP COLUMN output_tokens, DROP COLUMN input_tokens,
    DROP COLUMN model, DROP COLUMN redacted_phones, DROP COLUMN redacted_emails, DROP COLUMN redacted_cards,
    DROP COLUMN ocr_chars, DROP COLUMN parse_reasons, DROP COLUMN parse_outcome, DROP COLUMN parsed_at;
