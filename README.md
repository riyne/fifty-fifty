# Fifty Fifty

Group expense splitting: photograph a receipt, an LLM itemizes it, friends claim their items, and balances are tracked and settled through a **simulated** payment provider.

> **Status: in development (milestones M0-M5 of 9 are done; M6 is in progress: item edits are built, the receipt pipeline is next).** Built and tested: Go API, OIDC auth (Dex), groups and join codes, expenses with claims, finalize and void into an append-only double-entry ledger, group balances and suggested transfers, idempotency keys, an authorization (BOLA) test suite, a Postgres job queue with a worker, a mock LLM and recorded crash-recovery, throughput, latency and retry benchmarks, and **simulated** settlement: idempotent payments through a mock provider with reconciliation, a watchdog, signed webhooks, end-to-end fault-injection tests with real processes, and a recorded fault-injection benchmark; and a concurrent ledger stress test through the real API with retries, duplicate idempotency keys and races, recorded on the production build, plus negative controls that switch off one lock or check at a time (only in a separate `unsafe_controls` build that is never shipped) and report what breaks or which database layer caught it. A CI workflow (formatting, vet, generated-code check, tests against Postgres, vulnerability scan) is written and passes locally via `make ci` but has not yet run on GitHub. Not built yet: the receipt pipeline, mobile app, lint and secret scanning. Payer-only item edits with an optimistic version lock exist (tested; not yet part of the stress run). See [docs/DESIGN.md](docs/DESIGN.md) for the plan and [docs/CLAIMS.md](docs/CLAIMS.md) for what is and is not claimed.

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

    go run ./cmd/worker run --help          # the queue worker (PAYMENTS_URL enables settlement jobs)
    go run ./cmd/mockpay --help             # the mock payment provider (settlement is simulated)
    make ci              # what CI runs: gofmt, vet (both builds), generate check, tests with the database required, govulncheck (about 4 minutes)
    make test-unsafe     # the negative-control seams, and the whole suite once with the unsafe_controls tag (nothing disabled)
    make test-controls   # negative controls at smoke size: real processes, about 6 minutes
    make bench-recovery  bench-queue  bench-settlement  bench-stress   # benchmarks (clean git tree required; see bench/README.md)

`make generate` regenerates the sqlc code after editing `migrations/` or `internal/db/queries/`.

## Known limits

- The rate limiter is in memory, per API process.
- The queue is Postgres-only by design; it will not scale to very high job rates.
- Settlement is simulated: the payment provider is a mock, no money moves. Reconciliation timings come from placeholder constants (first check 5 s, doubling to 5 minutes, 12 checks), not from a tuned design.
- The settlement benchmark inserts settlements directly and does not measure the API's create path (auth, group lock, capacity check); the stress run does exercise that path, but with no throughput claim.
- The stress run never races a finalize or void against a settlement in the same group (settle groups are frozen and separate), and the recorded runs predate item edits: the workload and the negative controls for the edit lock and the version check exist since M6.2, but are not recorded until M6.3.
- The `unsafe_controls` build can remove a lock or a check. It exists only for the negative-control runs and must never be deployed; the production binaries contain none of that code (tested).
- The payment webhook endpoint exists only when `PAYMENTS_WEBHOOK_SECRET` is set; it has no rate limit and no IP allow-list.
