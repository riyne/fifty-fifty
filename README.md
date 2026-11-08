# Fifty Fifty

Group expense splitting: photograph a receipt, an LLM itemizes it, friends claim their items, and balances are tracked and settled through a **simulated** payment provider.

> **Status: in development. Milestones M0-M8 of 9 are done; the real-model accuracy evaluation and the full README (M9) are not.** The full README (architecture, demo) is M9; this page is a working summary.
>
> Built and tested (the evidence for each is in [docs/CLAIMS.md](docs/CLAIMS.md), the reasons in [docs/DESIGN.md](docs/DESIGN.md)):
> - **Ledger:** Go API, OIDC auth (Dex), groups and join codes, expenses with claims, payer-only item edits with an optimistic version lock, finalize and void into an append-only double-entry ledger, group balances and suggested transfers, idempotency keys, an authorization (BOLA) suite.
> - **Concurrency evidence:** a concurrent stress test through the real API (retries, duplicate idempotency keys, races, item edits) recorded on the production build, plus negative controls that switch off one lock or check at a time (only in a separate `unsafe_controls` build that is never shipped) and report what breaks or which database layer caught it.
> - **Row-level security** (defense in depth): Postgres policies, forced on every table that holds group or user data, limit the application role to the rows of the caller's groups even for a query with no membership check of its own; tested on a scratch database with a detector that shows the tests notice a missing policy (CLAIMS row 11). It covers membership of the group only, not the worker, the payment webhook or first-login (which run as their own roles), and the caller's identity is a setting the application role could set to anyone. Overhead was measured at the database layer on one machine: the policies cost about 18% of a mixed read and write workload's throughput and the identity seam more than that, mostly from extra round trips (CLAIMS row 11).
> - **Queue:** a Postgres job queue and worker with recorded crash-recovery, throughput, latency and retry benchmarks against a mock LLM.
> - **Settlement, simulated:** idempotent payments through a mock provider with reconciliation, a watchdog, signed webhooks, fault-injection tests with real processes and a recorded fault-injection benchmark.
> - **Receipts:** upload with image hardening, a content-addressed object store and a daily quota; a `receipt.parse` job (real Tesseract OCR, redaction of card numbers, phone numbers and email addresses, an LLM extractor behind an interface, strict validation, review hand-off, a sweep for dead jobs); synthetic receipt generator. Tested end to end with real processes (including a SIGKILLed worker) **against a mock LLM**.
> - **Redaction recall**, measured on synthetic receipts: 100% on perfect text, 96.1% on Tesseract's reading of clean renders, 72.7% on the most distorted; emails are the weak spot (CLAIMS row 9). Synthetic data, so no statement about real receipts.
> - **Mobile (iOS Simulator only):** a React Native (Expo) client for the whole flow against the unchanged api: sign in with Auth Code + PKCE against Dex, groups and join codes, expenses with claim, finalize (with "split the rest") and void, add a receipt from the photo library (turned upright and shrunk before upload, polled while it parses), review and confirm, and a simulated settlement followed to a final state. The client is typed from `api/openapi.yaml` with a drift check, and its pure logic has Jest tests; the screens were run by hand on an iPhone simulator against the mock LLM and the mock payment provider. No Android, no physical device, no camera, no automated UI tests, and it is not in CI (CLAIMS rows 12 and 6; row 6, the login flow, stays `planned` because no automated test covers it).
> - **CI:** a workflow (formatting, vet, generated-code check, tests against Postgres and Tesseract, vulnerability scan) passes on GitHub Actions (green run confirmed 2026-11-07) and locally with `make ci`. The end-to-end tests, negative controls and benchmarks are not run in CI.
>
> Not built: a live call to the real model (none has ever been made), an Android app or any UI automation, lint and secret scanning.

- Synthetic data only. No real money moves anywhere; the payment provider is a mock.
- How it fits together (processes, database setup and roles, tables, the queue, locking, idempotency, the ledger): [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).
- Every number or capability claimed about this project must appear in [docs/CLAIMS.md](docs/CLAIMS.md) with its evidence. Benchmark method and raw results: [bench/README.md](bench/README.md).

## Run locally

Needs Go 1.27+ and Docker. Everything below is dev-only with throwaway credentials.

    make demo            # the whole system on a fresh throwaway database, walked through as alice, bob and carol (needs Docker, Tesseract, port 8080 free; mock LLM and mock payment provider)
    make demo-keep       # the same, then leave it running for the iOS app until Ctrl-C
    make up              # Postgres (localhost:5433) and Dex (localhost:5556)
    make migrate-up      # apply migrations
    make test            # unit + integration tests (integration tests need the database above)
    make test-e2e        # real processes on scratch databases: settlement scenarios, the receipt pipeline with a SIGKILLed worker, a stress smoke test (about 2 minutes)
    make run-api         # API on :8080 (the Makefile exports DATABASE_URL and SYSTEM_DATABASE_URL: the api uses two database roles, fifty_app for users and fifty_system for first login and the payment webhook)

    TOKEN=$(scripts/dev-token.sh alice)     # alice, bob or carol; password is "password"
    curl -H "Authorization: Bearer $TOKEN" localhost:8080/v1/me

    DATABASE_URL=postgres://fifty_worker:fifty@localhost:5433/fifty?sslmode=disable go run ./cmd/worker run --help   # the queue worker; it connects as its own role, fifty_worker, not fifty_app (PAYMENTS_URL enables settlement jobs; RECEIPT_DIR plus ANTHROPIC_BASE_URL or ANTHROPIC_API_KEY enables receipt parsing, which needs `brew install tesseract`)
    go run ./cmd/receiptgen --help          # synthetic receipts (images, text, ground truth) for demos and the mock LLM
    go run ./cmd/mockllm --help             # the mock LLM (--fixtures answers receipts from their ground truth)
    go run ./cmd/mockpay --help             # the mock payment provider (settlement is simulated)
    make ci              # what CI runs: gofmt, vet (both builds), generate check, tests with the database required, govulncheck (about 5.5 minutes)
    make test-unsafe     # the negative-control seams, and the whole suite once with the unsafe_controls tag (nothing disabled)
    make test-controls   # negative controls at smoke size: real processes, about 6 minutes
    make bench-recovery  bench-queue  bench-settlement  bench-stress  bench-redaction  bench-rls   # benchmarks (clean git tree required; see bench/README.md)

`make generate` regenerates the sqlc code after editing `migrations/` or `internal/db/queries/`.

### The mobile client (iOS Simulator)

Needs macOS with Xcode, Node and CocoaPods (`brew install cocoapods`), and the `make up`, `make migrate-up` and `make run-api` above. The simulator shares the host's network, so the app reaches the api at `localhost:8080` and Dex at `localhost:5556` (`EXPO_PUBLIC_API_URL` and `EXPO_PUBLIC_OIDC_ISSUER` override them).

    cd mobile && npm install
    npx expo run:ios           # builds the development build (scheme fiftyfifty://), starts Metro, opens the simulator
    make mobile-check          # typecheck, check that the generated API types match api/openapi.yaml, Jest (not part of make ci)
    make mobile-generate       # regenerate mobile/src/api/schema.d.ts after changing api/openapi.yaml

Sign in as alice, bob or carol (password `password`). Two more processes make the receipt and settlement screens work (everything is a mock, nothing real is called):

- **Receipts:** a worker that can read them: `RECEIPT_DIR` (the Makefile's default for the api is `~/.fifty-fifty-receipts`), Tesseract, and `ANTHROPIC_BASE_URL` pointing at `go run ./cmd/mockllm --fixtures <dir>/fixtures.json`, with the receipts made by `go run ./cmd/receiptgen --out <dir> ...`. Put images in the simulator's photo library with `xcrun simctl addmedia booted <image>`. Only receipts from `receiptgen` parse into items (the mock answers from the `REF n` line that OCR reads); any other image goes to review.
- **Settlement:** `go run ./cmd/mockpay` and `PAYMENTS_URL=http://127.0.0.1:8090` on the worker.
- **Use a fresh database** (`make reset`, then `make up` and `make migrate-up`). A worker with `PAYMENTS_URL` pays every old `pending` settlement it finds, and a database that has had benchmark or stress runs can hold thousands.

## Known limits

- The rate limiter is in memory, per API process.
- Row-level security limits the user-request path to the caller's groups and nothing more: the worker, the payment webhook and first-login run as other database roles, and the caller's identity is a transaction setting that code running as the application role could set to anyone. The identity seam adds three round trips to every autocommit read, which on a local database roughly halves read throughput (CLAIMS row 11); batching reads into one round trip is designed and tabled.
- The queue is Postgres-only by design; it will not scale to very high job rates.
- Settlement is simulated: the payment provider is a mock, no money moves. Reconciliation timings come from placeholder constants (first check 5 s, doubling to 5 minutes, 12 checks), not from a tuned design.
- The settlement benchmark inserts settlements directly and does not measure the API's create path (auth, group lock, capacity check); the stress run does exercise that path, but with no throughput claim.
- The stress run never races a finalize or void against a settlement in the same group (settle groups are frozen and separate). Item edits are in the recorded stress mix (they only reprice; adding or removing items and confirm are not), with negative controls for the edit lock and the version check.
- The `unsafe_controls` build can remove a lock or a check. It exists only for the negative-control runs and must never be deployed; the production binaries contain none of that code (tested).
- Receipt upload exists only when `RECEIPT_DIR` is set, stores images on the local disk of one host (no delete, no sweep of unreferenced images), and drops the EXIF orientation instead of applying it, so the client must send upright pixels (the mobile client does). An uploaded expense stays in `parsing` until a worker that can run the parse job handles it (the worker needs `RECEIPT_DIR`, Tesseract and a model: the mock LLM or the real API); after 15 minutes without a live job a sweep moves it to review.
- The mobile client is built for and run on the iOS Simulator only, by hand: there are no automated UI tests, no Android build, no camera (photos come from the library) and no mobile job in CI. The ID token is the bearer and there is no refresh token, so an expired session goes back to login. The review screen cannot show the receipt photo, because the api has no endpoint that returns the stored image. Editing a `claiming` expense and creating an expense without a receipt are not in the app.
- The payment webhook endpoint exists only when `PAYMENTS_WEBHOOK_SECRET` is set; it has no rate limit and no IP allow-list.
- The real Anthropic API has never been called. Every extraction test uses a mock LLM that never reads its prompt, so nothing here says how a real model does on receipts; there is no accuracy figure. [docs/LIVE_SMOKE.md](docs/LIVE_SMOKE.md) is the procedure for a first live check (skipped for now).
