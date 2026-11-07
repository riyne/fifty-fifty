# Live smoke test against the real Anthropic API (optional, not done)

Status: **not run.** Ryan decided to skip it at the end of M6 (2026-11-07) and keep this procedure for later. Until it is run, nothing in this project says anything about how the real API or a real model behaves: every receipt test uses a mock LLM (`internal/mockllm`), which never reads its prompt. CLAIMS rows 27 and 28 say so.

## What it is for

One small, supervised check that the pipeline works against the real service, run once before relying on the extractor for anything (the M9 accuracy eval, CLAIMS row 10, or the demo). It can find things the mock cannot:

- the request is accepted (model name, `max_tokens`, `temperature`, headers, `anthropic-version`);
- the model answers in the shape `receipts.ParseExtraction` accepts, or adds prose, fences, extra fields or a different number format (the strict validator would send those receipts to `needs_review`, which is a finding about the prompt, not a bug);
- 429s carry a `retry-after` and the worker waits for it;
- token counts and cost per receipt are what the plan assumed.

It is **not** an accuracy evaluation. A handful of receipts says nothing about item-level accuracy; that is M9 (about 50 synthetic receipts, with the review-flag catch rate).

## Before you start

1. **Set a spend limit in the Anthropic console first.** The cost cap decided for M6 is the console limit plus `max_tokens` (4096 by default, `internal/anthropic`), the daily upload quota (20 a day per user) and the token counts recorded per receipt. There is no budget counter in the database. Pick a limit you would be content to lose, a few dollars at most.
2. **Use a key made for this**, put it in the shell only (`export ANTHROPIC_API_KEY=...`), never in a file in the repo, never in a command history you share. Rotate it afterwards if it was pasted anywhere.
3. **Synthetic receipts only.** The redacted OCR text of every receipt you upload is sent to Anthropic. Never upload a real receipt, yours or anyone else's. Redaction removes card numbers, phone numbers and email addresses, not names or addresses (CLAIMS row 9).
4. Check the model name. The default is `claude-haiku-5-5`; confirm it against the current model list, and set `ANTHROPIC_MODEL` if it differs. Check current pricing on the pricing page; do not rely on any figure written in this repo.
5. Tesseract installed (`brew install tesseract`), `make up && make migrate-up`.

## Procedure (about 15 minutes)

Use a scratch directory outside the repo for the images.

    export ANTHROPIC_API_KEY=...              # this shell only
    go run ./cmd/receiptgen --out ~/fifty-smoke --from 1 --count 10      # NNN.png, NNN.txt, NNN.json (ground truth)

    # terminal 1: the api (RECEIPT_DIR defaults to ~/.fifty-fifty-receipts in the Makefile)
    make run-api

    # terminal 2: a worker that can read receipts (it needs the same RECEIPT_DIR as the api)
    export DATABASE_URL=postgres://fifty_app:fifty@localhost:5433/fifty?sslmode=disable
    export RECEIPT_DIR=$HOME/.fifty-fifty-receipts
    go run ./cmd/worker run                   # logs "receipt.parse enabled" with the model and base URL

    # terminal 3: upload, one at a time at first
    TOKEN=$(scripts/dev-token.sh alice)
    GROUP=$(curl -s -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
            -d '{"name":"smoke","currency":"CAD"}' localhost:8080/v1/groups | jq -r .id)
    curl -s -H "Authorization: Bearer $TOKEN" -H "Idempotency-Key: $(uuidgen)" \
         -F file=@$HOME/fifty-smoke/000000001.png localhost:8080/v1/groups/$GROUP/receipts
    # poll: curl -s -H "Authorization: Bearer $TOKEN" localhost:8080/v1/groups/$GROUP/expenses/<expense_id> | jq

Upload the first receipt alone and watch the worker log before uploading the rest. If the first attempt fails with `HTTP 400 (invalid_request_error)`, stop and look: the likeliest causes are the model name or a request field the model does not accept (for example `temperature`, set in `internal/anthropic/client.go`). `HTTP 401` is the key. The worker retries four times, then the receipt ends in `needs_review` with reason `extractor_failed`; this is the designed behavior, so a wrong key looks like a pile of such receipts.

Then upload the other nine. For each, compare the expense with the receipt's `NNN.json`.

## What to look at

    psql ... -c "SELECT r.id, r.parse_outcome, r.parse_reasons, r.model, r.input_tokens, r.output_tokens, r.ocr_chars
                 FROM receipts r JOIN expenses e ON e.id = r.expense_id WHERE e.group_id = '<group>' ORDER BY r.created_at"

- How many ended `claiming` and how many `needs_review`, and why (`invalid_output` means the answer was not the expected shape; the model's raw answer is deliberately not stored or logged, so to see it, reproduce with a temporary log in a scratch branch, not in the repo).
- Whether the claiming ones match the ground truth items and total exactly (they add up by construction of the check, which does not mean they are right).
- Token counts per receipt, to compute the real cost per receipt from current prices.
- Whether any 429 occurred and the retry came after the `retry-after`.

## Recording the result

If you run it, add a dated row to the DESIGN decision log ("Live smoke (date)") with: commit, model, number of receipts, outcomes by reason, token totals, what broke or surprised, what was changed (a prompt change is a code change with its own commit). Say plainly that it is a smoke test. Do not add accuracy numbers to CLAIMS from it; at most, once it has run, CLAIMS row 27 can say the client was exercised against the real API on that date, with what was seen. Row 10 (accuracy) stays `planned` until the M9 evaluation.

## Afterwards

Stop the worker. Revoke or rotate the key. The smoke receipts and expenses stay in the development database (parsed or not); delete `~/fifty-smoke` and, if you like, `~/.fifty-fifty-receipts` (images are content-addressed and shared; nothing else depends on them).
