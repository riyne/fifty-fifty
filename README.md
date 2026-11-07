# Fifty Fifty

Group expense splitting: photograph a receipt, an LLM itemizes it, friends claim their items, and balances are tracked and settled through a **simulated** payment provider.

> **Status: in development (milestones M0-M6 of 9 are done; mobile, the accuracy evaluation and the full README are not).** Built and tested: Go API, OIDC auth (Dex), groups and join codes, expenses with claims, finalize and void into an append-only double-entry ledger, group balances and suggested transfers, idempotency keys, an authorization (BOLA) test suite, a Postgres job queue with a worker, a mock LLM and recorded crash-recovery, throughput, latency and retry benchmarks, and **simulated** settlement: idempotent payments through a mock provider with reconciliation, a watchdog, signed webhooks, end-to-end fault-injection tests with real processes, and a recorded fault-injection benchmark; and a concurrent ledger stress test through the real API with retries, duplicate idempotency keys and races, recorded on the production build, plus negative controls that switch off one lock or check at a time (only in a separate `unsafe_controls` build that is never shipped) and report what breaks or which database layer caught it. A CI workflow (formatting, vet, generated-code check, tests against Postgres, vulnerability scan) is written and passes locally via `make ci` but has not yet run on GitHub. Receipt upload with image hardening, a content-addressed object store and a daily quota exists, and the parse job (redaction, validation, review hand-off, a sweep for dead jobs) is built and tested with fakes and, for the model call, a plain-HTTP Anthropic client tested against a mock LLM (the real API has not been called). Real OCR (Tesseract, which must be installed: `brew install tesseract`) is wired into the worker binary and tested end to end with real processes, with a mock model. Regex redaction of card numbers, phone numbers and email addresses is measured on synthetic receipts (100% on perfect text, 96.1% on Tesseract's reading of clean renders, 72.7% on the most distorted; emails are the weak spot; synthetic data, so no statement about real receipts; see CLAIMS row 9). Not built yet: a live call to the real model, mobile app, lint and secret scanning. Payer-only item edits with an optimistic version lock exist, are tested, and are part of the recorded stress run and its negative controls. See [docs/DESIGN.md](docs/DESIGN.md) for the plan and [docs/CLAIMS.md](docs/CLAIMS.md) for what is and is not claimed.

- Synthetic data only. No real money moves anywhere; the payment provider is a mock.
- Every number or capability claimed about this project must appear in [docs/CLAIMS.md](docs/CLAIMS.md) with its evidence. Benchmark method and raw results: [bench/README.md](bench/README.md).

## Run locally

Needs Go 1.27+ and Docker. Everything below is dev-only with throwaway credentials.

    make up              # Postgres (localhost:5433) and Dex (localhost:5556)
    make migrate-up      # apply migrations
    make test            # unit + integration tests (integration tests need the database above)
    make test-e2e        # settlement end to end: real worker and mock-provider processes, scratch databases (a few minutes)
    make run-api         # API on :8080

    TOKEN=$(scripts/dev-token.sh alice)     # alice, bob or carol; password is "password"
    curl -H "Authorization: Bearer $TOKEN" localhost:8080/v1/me

    go run ./cmd/worker run --help          # the queue worker (PAYMENTS_URL enables settlement jobs; RECEIPT_DIR plus ANTHROPIC_BASE_URL or ANTHROPIC_API_KEY enables receipt parsing, which needs `brew install tesseract`)
    go run ./cmd/receiptgen --help          # synthetic receipts (images, text, ground truth) for demos and the mock LLM
    go run ./cmd/mockllm --help             # the mock LLM (--fixtures answers receipts from their ground truth)
    go run ./cmd/mockpay --help             # the mock payment provider (settlement is simulated)
    make ci              # what CI runs: gofmt, vet (both builds), generate check, tests with the database required, govulncheck (about 4 minutes)
    make test-unsafe     # the negative-control seams, and the whole suite once with the unsafe_controls tag (nothing disabled)
    make test-controls   # negative controls at smoke size: real processes, about 6 minutes
    make bench-recovery  bench-queue  bench-settlement  bench-stress  bench-redaction   # benchmarks (clean git tree required; see bench/README.md)

`make generate` regenerates the sqlc code after editing `migrations/` or `internal/db/queries/`.

## Known limits

- The rate limiter is in memory, per API process.
- The queue is Postgres-only by design; it will not scale to very high job rates.
- Settlement is simulated: the payment provider is a mock, no money moves. Reconciliation timings come from placeholder constants (first check 5 s, doubling to 5 minutes, 12 checks), not from a tuned design.
- The settlement benchmark inserts settlements directly and does not measure the API's create path (auth, group lock, capacity check); the stress run does exercise that path, but with no throughput claim.
- The stress run never races a finalize or void against a settlement in the same group (settle groups are frozen and separate). Item edits are in the recorded stress mix (they only reprice; adding or removing items and confirm are not), with negative controls for the edit lock and the version check.
- The `unsafe_controls` build can remove a lock or a check. It exists only for the negative-control runs and must never be deployed; the production binaries contain none of that code (tested).
- Receipt upload exists only when `RECEIPT_DIR` is set, stores images on the local disk of one host (no delete, no sweep of unreferenced images), and drops the EXIF orientation instead of applying it, so the client must send upright pixels. An uploaded expense stays in `parsing` until a worker that can run the parse job (it needs OCR and a model, not wired in yet) handles it; after 15 minutes without a live job a sweep moves it to review.
- The payment webhook endpoint exists only when `PAYMENTS_WEBHOOK_SECRET` is set; it has no rate limit and no IP allow-list.
- The real Anthropic API has never been called. Every extraction test uses a mock LLM that never reads its prompt, so nothing here says how a real model does on receipts; there is no accuracy figure. [docs/LIVE_SMOKE.md](docs/LIVE_SMOKE.md) is the procedure for a first live check (skipped for now).
