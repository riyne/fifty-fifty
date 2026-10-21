# Fifty Fifty: design

Status: approved decisions as of 2026-10-20. Update this file when a decision changes; don't let it drift from the code.

**One line:** photograph a receipt, an LLM itemizes it, friends claim their items, and the app tracks net balances and records settlements through a simulated payment provider.

## 1. Goals and non-goals

Goals (what this project is meant to demonstrate, in priority order):
1. Correctness under concurrency: many users acting on shared state, with measured invariants and negative controls.
2. An async job pipeline that survives worker crashes (leases, fencing, retries, dead-letter), with measured recovery.
3. Idempotent, failure-tolerant settlement against a flaky external provider.
4. Sound authentication/authorization practice (OIDC, group-scoped authz, tested for cross-group access).
5. An LLM used as a pluggable, untrusted dependency (validated output, redaction, mockable).

Non-goals / explicitly cut: real money, multi-currency, multi-payer expenses, push notifications, email/SMS invites, application-level encryption of receipts, Kubernetes/Terraform/autoscaling (README "extension path" only), vision-LLM extraction, native Swift/Kotlin.

The ledger records IOUs between friends, not real funds. Never describe it as a financial or payments ledger.

## 2. Decision log

| Area | Decision |
|---|---|
| Time | Nominal 20+ h/week; plan at 15. No fixed date; core (M1-M5) is the commitment |
| Backend | Go, Postgres, pgx + sqlc (no ORM), REST/JSON, OpenAPI spec |
| Queue | Own Postgres queue (`FOR UPDATE SKIP LOCKED`, leases, heartbeats, fencing token, backoff, DLQ). Not River, not Kafka/Redis/SQS |
| Job status to app | Polling |
| Money | int64 minor units; one currency per group; CAD and USD only |
| Ledger | Double-entry, append-only, balances derived by SUM; net balance per member per group |
| Finalize | Blocked until every item has a claimant; payer may "split the rest evenly" among chosen members |
| Settlement | Async via queue; mock provider with fault injection; reconciliation; ledger posts only on confirmed success |
| Receipt pipeline | Tesseract OCR -> regex redaction -> text LLM, behind an `Extractor` interface |
| LLM | Anthropic API for demo and ~50-receipt accuracy eval; standalone mock LLM server for all benchmarks |
| Auth | Self-hosted Dex (static synthetic users); Auth Code + PKCE; expiring/revocable/rate-limited join codes; in-process fake issuer in tests (no auth-bypass flag) |
| Security | Baseline in section 9; RLS as a post-benchmark stretch (M7); envelope encryption cut |
| Mobile | React Native + Expo (TypeScript), 3-4 screens, thin client, iOS Simulator + Android emulator only |
| CI | GitHub Actions |
| Authz shape (M1.2) | Memberships carry a role (`owner` \| `member`; creator is owner). Group-scoped reads go through one query that joins `memberships`, so fetch and authorization are the same statement; non-member and nonexistent both return an identical 404. Users are provisioned on first valid token (read first, race-safe insert only for new users). Tokens are verified by `go-oidc` (RS256 only) against the issuer's JWKS; tests use an in-process fake issuer that the unmodified verifier checks, so there is no bypass path. Integration tests need Postgres (`TEST_DATABASE_URL`); they skip when unset, so CI must set it |
| Join codes (M1.3) | 12 random Crockford-base32 chars (60 bits), shown once, stored as unsalted SHA-256 (fine for a high-entropy random secret; protection is expiry + use limit + rate limit, not hash cost). Owner-only create/list/revoke; defaults 5 uses / 72 h, max 50 / 168 h. Redemption locks the code row (`FOR UPDATE`) and spends a use only when the user is actually added. Unknown, expired, revoked and used-up codes are indistinguishable (404). Member-but-not-owner gets 403; non-member gets 404. Redeeming requires `Idempotency-Key` |
| Rate limiting (M1.3) | In-memory token bucket per user on `POST /v1/join` (burst 10, then 1 per 6 s). Per api process and reset on restart; correct for the single-instance deployment, multiplies with replicas. README must say so |
| Audit (M1.3) | `audit_events` written in the same transaction as the action: `join_code.created`, `join_code.revoked`, `member.joined`. No secrets in `data`. Failed redemptions are not audited (rate limiter + logs instead) |
| Toolchain (M1.1) | Module `github.com/riyne/fifty-fifty`; Go 1.27.3; Postgres 17 (host port 5433); Dex v2.46.0 (config baked into image, no bind mounts); goose v3.29.0 pinned in the separate `tools/go.mod` module and run via `go tool -modfile=tools/go.mod goose` (keeps its large dependency tree out of the app module and out of govulncheck scope) |
| Repo | Private until M5 numbers and README exist, then public |

## 3. Architecture

docker-compose services:

- `api`: Go HTTP server (REST/JSON). Validates JWTs, enforces authorization, owns all writes.
- `worker`: Go binary from the same module. Runs queue consumers (receipt parsing, settlement execution, reconciliation).
- `postgres`: primary store, queue, idempotency keys, audit events.
- `dex`: OIDC provider with static users.
- `mock-llm`: standalone Go HTTP server using the real provider's wire format; configurable latency distribution, 429/5xx/timeout rate, malformed-JSON rate; deterministic output from synthetic ground truth.
- `mock-payments`: standalone Go HTTP server; configurable latency, failure, timeout-after-success, duplicate/out-of-order webhooks (HMAC-signed).
- Receipt image storage: behind an `ObjectStore` interface. Backend (filesystem volume vs MinIO) decided at M6.

Mobile app (`mobile/`, Expo) talks only to `api` and Dex (system browser for login).

Repo layout. Built (as of M1.3):

    api/openapi.yaml                 the spec; test/bola reads it to check authz coverage
    cmd/api/                         HTTP server entrypoint
    internal/auth/                   OIDC verifier + middleware + first-login provisioning
    internal/auth/authtest/          in-process fake issuer (tests only; nothing in prod imports it)
    internal/db/                     sqlc output (committed) + queries/*.sql (source)
    internal/groups/                 group + join-code handlers
    internal/httpapi/                root handler, health, /v1 wiring
    internal/httpx/                  JSON read/write helpers
    internal/idempotency/            idempotency-key helper (claim-in-transaction)
    internal/joincode/               join code generate/normalize/hash
    internal/ratelimit/              in-memory per-user token bucket
    internal/teststack/              whole api in-process for integration tests
    internal/testutil/               test DB pool (TEST_DATABASE_URL), unique subjects
    migrations/                      goose SQL migrations (sqlc reads these as its schema)
    deploy/docker-compose.yml        Postgres 17 + Dex; deploy/dex/ (config baked into image)
    scripts/dev-token.sh             real Auth Code + PKCE login against Dex, prints an ID token
    tools/go.mod                     separate module pinning goose and sqlc
    sqlc.yaml  Makefile
    test/bola/                       BOLA suite

Planned, not built yet:

    cmd/{worker,mockllm,mockpay,stress}/
    internal/{expenses,ledger,queue,receipts,settlements,...}/
    bench/   (scripts, raw results committed, methodology)
    test/    (concurrency, rls suites)
    mobile/

## 4. Domain model (conceptual)

- `users` keyed by (issuer, subject); provisioned on first valid token.
- `groups` (one currency), `memberships`, `join_codes` (hashed, expiring, revocable, use-limited).
- `expenses`: payer, totals, status, `version`.
  - Status: `parsing` -> (`needs_review`) -> `claiming` -> `finalized` | `void`.
  - An expense can also be created directly with items (needed for stress tests and manual entry); receipt upload is one way to produce items.
- `expense_items`, `claims` (unique per item+member).
- `ledger_transactions` (kind: expense | settlement | void) and `ledger_entries` (signed amounts per member). Append-only.
- `settlements`: status `pending` -> `succeeded` | `failed` | `unknown`.
- `idempotency_keys`: scoped per user, with request fingerprint and stored response.
- `jobs`: see section 6.
- `audit_events`: finalize, void, settle, membership changes, join-code use.

## 5. Ledger and concurrency

Money: int64 minor units, CAD/USD only (2 decimals). No floats anywhere.

Postings:
- Finalize: payer is credited the expense total; each claimant is debited their share (payer included if they claimed items). Entries sum to zero per transaction.
- Settlement success: payer-of-debt credited, payee debited by the settled amount.
- Void: reversing entries for the original transaction.
- Group net balances = SUM over entries; they always net to zero. Suggested transfers come from simplification (greedy over net balances, read-time, suggestions only; reduces transfer count, does not minimize).

Share calculation:
- Shared items split evenly among claimants; remainders by largest-remainder with deterministic tie-break (member id).
- Tax and tip are prorated by each member's claimed subtotal using the same allocator.
- Property tests assert: shares always sum exactly to the total; allocator is deterministic.

Concurrency (READ COMMITTED + targeted locks; SERIALIZABLE is not used):
- Claim / unclaim: `FOR SHARE` on the expense row, then verify status = `claiming`. Concurrent claimers don't block each other.
- Finalize / void / item edits: `FOR UPDATE` on the expense row. Finalize waits for in-flight claims, then reads a stable claim set. Claims arriving after wait, see `finalized`, and are rejected (409).
- Item edits (payer only) use the `version` column as an optimistic lock (conditional update; zero rows -> 409).
- Settlement creation: validated under a per-group lock (lock the group row). Available amount = what the payer owes, bounded by what the payee is owed, minus the sum of pending settlements. Success posting does not re-validate (it records a payment that already happened).
- DB-enforced invariants: unique constraints, deferred constraint that each ledger transaction sums to zero, app role has no UPDATE/DELETE on ledger tables (plus a trigger and a test).
- Considered, not adopted: bumping `version` on every claim so finalize is a compare-and-swap on the reviewed state. It would serialize claimers on one row. Revisit if UX needs it.

Idempotency: required `Idempotency-Key` header on create-expense, finalize, void, create-settlement, join-group.
- Same key + same fingerprint: replay the stored response (a concurrent duplicate waits on the unique index and then replays).
- Same key + different fingerprint: 422.
- Keys expire after 24 h.
- Implemented (M1.3) as `internal/idempotency`: the key row is claimed in the same transaction as the operation and completed before commit, so effects, stored response and key commit or roll back together (no "in progress" state). Every response below 500 is stored and replayed, including business-rule 4xx (Stripe model: a key's first outcome is final). 5xx and errors roll back and are not stored, since the transaction is atomic and a retry should be able to succeed. Validation errors, 401 and 429 are rejected before the key is claimed. Handlers that return a non-2xx result must not have written anything they don't want committed.

Negative controls: the version check, row locks, and idempotency check can each be disabled only in a test binary built with the `unsafe_controls` tag. There is no runtime flag in the production binary.

## 6. Job queue

Single `jobs` table: kind, payload, state (`queued` | `running` | `succeeded` | `dead`), attempts, max_attempts, run_at, lease_token, lease_expires_at, last_error.

- Claim: one statement selects eligible rows (`queued` and due, or `running` with an expired lease) with `FOR UPDATE SKIP LOCKED`, sets a fresh lease token and expiry, increments attempts.
- Heartbeat extends the lease only if the token still matches.
- Completion/failure writes are conditional on the lease token (fencing). A stale worker cannot overwrite a newer attempt.
- Handler effects and job completion commit in one transaction, so effects are exactly-once with respect to Postgres. The LLM/provider call itself is at-least-once (may be billed twice on retry); this is documented, not hidden.
- Retries: exponential backoff with jitter up to max_attempts, then `dead` (dead-letter state with requeue by an operator command).
- Enqueue happens in the same transaction as the row that needs the job (no dual write).
- Metrics: queue depth, oldest-job age, attempts histogram, exposed on a metrics endpoint.
- Optional (measure first): `LISTEN/NOTIFY` to wake workers instead of pure polling.

Why not River/SQS/Kafka (be ready to say this in an interview): transactional enqueue with no extra infrastructure; the mechanism is the point of the exercise; River would hide it. Trade-off accepted: a Postgres queue won't scale to very high job rates, and the README will say so.

## 7. Settlement flow

1. `POST /settlements` with `Idempotency-Key`: validate under group lock, insert `pending`, enqueue `settlement.execute` in the same transaction, return 202.
2. Worker calls mock provider using the settlement id as the provider idempotency key.
3. Outcomes: success -> in one transaction: settlement `succeeded` (conditional on current status) + ledger postings. Definite failure -> `failed`. Timeout/ambiguous -> `unknown` and a `settlement.reconcile` job queries the provider by idempotency key with backoff.
4. Provider webhooks (HMAC-verified) may arrive duplicated or out of order; handling is idempotent and status transitions are monotonic.
5. Ledger posts exactly once per settlement, only on confirmed success.

Mock provider fault injection: latency, definite failures, timeout after the charge succeeded, duplicate webhooks, out-of-order webhooks.

## 8. Receipt pipeline

upload -> hardening (size/dimension limits, sniff real content type, strip EXIF, re-encode) -> store -> `receipt.parse` job:
Tesseract OCR -> regex redaction (card numbers, phones, emails; names are not redacted) -> `Extractor` (text LLM via provider interface; mock or Anthropic) -> JSON-schema validation -> reconciliation check -> items written, expense moves to `claiming` (or `needs_review`).

- Review flag is deterministic: schema valid and items + tax + tip == total within tolerance. LLM self-reported confidence is not used.
- `needs_review`: payer edits items (optimistic lock), then proceeds.
- Quantity lines (e.g. 3 x beer) are expanded into per-unit items so units can be claimed individually (remainders by the same allocator). Details settled in M2.
- Prompt injection: receipt text is untrusted. The model has no tools; output is schema-validated and reconciled; worst case is a wrong parse the user reviews. Injection fixtures live in the test suite.
- Per-user rate limit and daily receipt quota (also caps LLM cost).
- Synthetic receipt generator renders receipts from known ground truth (items, totals, planted PII, noise/skew). It feeds the OCR/redaction evals, the accuracy eval, mock LLM responses, and the demo (print or display receipts and photograph them).

## 9. Auth and security

Baseline (always built):
- OIDC: backend validates JWT signature (JWKS), issuer, audience, expiry (`coreos/go-oidc`). Users keyed by (issuer, sub).
- Group-scoped authorization in the app layer; **BOLA/IDOR suite**: user in group A hits every endpoint with group B's IDs, expects 403/404.
- Append-only ledger enforced by the database; `audit_events` for authz-relevant actions.
- Join codes: hashed at rest, expiring, revocable, rate-limited; brute-force and expiry tests.
- Per-user rate limiting; daily receipt quota.
- CI: lint, govulncheck, secret scanning. Structured logs never contain receipt text or tokens.
- Parameterized queries only (sqlc); request size limits; input validation.

Stretch (M7): Postgres RLS on core tables as defense in depth.
- `SET LOCAL` per transaction (pooled connections must not leak user identity), `FORCE ROW LEVEL SECURITY`, app role is not the owner and not a superuser, workers use a separate role.
- Test: a handler with its membership check deliberately removed still returns zero cross-group rows. Measure overhead.

Mobile login: Auth Code + PKCE via the system browser; tokens in Keychain/Keystore. Known gotcha: the OIDC `iss` must match from every vantage point (iOS Simulator `localhost`, Android emulator `10.0.2.2` or `adb reverse`, backend container `dex`). Plan: one issuer URL, `adb reverse` on Android, and `go-oidc` split between issuer URL and discovery URL on the backend.

## 10. Testing and benchmarking plan

Every number in README/resume comes from a recorded run listed in `docs/CLAIMS.md`. Each benchmark records: machine, Docker resource limits, Postgres version, commit hash, parameters, number of runs, spread (not just best run), and raw output committed under `bench/results/`.

1. **Ledger stress:** N users over G groups, concurrent mix of create/claim/finalize/void/settle (with retries and duplicate keys), K seconds. Checks: global and per-group sum zero; each transaction zero; no duplicate settlement per idempotency key; ledger shares equal shares recomputed from claims; no accepted claim after finalize. Run twice: production binary (expect 0 anomalies) and `unsafe_controls` binary (expect anomalies, report counts).
2. **Queue throughput/latency:** jobs/s and end-to-end p50/p99 vs worker count (1, 2, 4, 8) against mock LLM with stated simulated latency.
3. **Crash recovery:** `kill -9` a worker mid-job over R trials; report time-to-recovery, lost jobs (expect 0), duplicate effects (expect 0).
4. **Retry behavior:** mock LLM with 429/5xx/malformed rates; success rate, attempts histogram, DLQ rate.
5. **Settlement fault injection:** timeout-after-success and duplicate webhooks; assert exactly-once posting; time to resolve `unknown`.
6. **Redaction recall** on planted synthetic PII (M6). **Accuracy eval** with real LLM on ~50 synthetic receipts (M9): item-level match, total match, review-flag catch rate. Synthetic data flatters results; the README says so.
7. **RLS overhead** (M7), with vs without.

Test layers: unit (allocator property tests), integration against real Postgres, concurrency suite, BOLA suite, fault-injection tests. CI runs invariant-only smoke versions of the stress test; full benchmarks run locally (CI timing is too noisy to quote).

## 11. Milestones (estimates are rough; re-baseline after M1)

Actual hours are not recorded anywhere; re-baselining needs Ryan's own time log.

| | Scope | Est. h |
|---|---|---|
| M0 | Design doc, scaffold. **Done** | 3 |
| M1 | Go module, compose (Postgres, Dex), migrations, sqlc, OIDC middleware, users/groups/join codes, CI skeleton. **M1.1 (module, compose, migrations, health), M1.2 (users/groups/memberships, sqlc, OIDC, fake issuer) and M1.3 (join codes, idempotency, rate limit, audit, first BOLA suite) done; M1.4 (CI) pending** | ~20 |
| M2 | Ledger core: expenses, claims, finalize, void, allocator, balances, simplification | ~25 |
| M3 | Queue: leases, fencing, backoff, DLQ, kill-a-worker recovery test and benchmark | ~20 |
| M4 | Settlement: idempotency, mock provider, async execution, reconciliation | ~20 |
| M5 | Stress test, negative controls, BOLA suite, benchmark write-up. **Resume-usable numbers (~5-7 weeks at 15 h/wk)** | ~20 |
| M6 | Receipt pipeline: generator, Tesseract, redaction, mock + Anthropic extractor, review flag | ~25 |
| M7 | RLS (cut first if late) | ~15 |
| M8 | Mobile: Expo, OIDC PKCE, group view, add receipt (picker), claim items, balances/settle | ~25 |
| M9 | Accuracy eval, README, architecture diagram, demo | ~15 |

Cut order if late: M7, then accuracy eval, then mobile polish.

## 12. Risks

- Scope: the full list is ~190 h. Core first; re-plan at each milestone.
- Velocity estimates are unmeasured guesses.
- Tesseract accuracy on messy photos; mitigated by `Extractor` interface and `needs_review` path.
- OIDC issuer mismatch across simulator/emulator/container (section 9).
- Android Studio/emulator setup is not done yet (only an SDK folder exists).
- LLM cost: capped by per-user quota; budget a few dollars.
- Defensibility: the author must be able to explain and rewrite the queue, locking, idempotency and ledger code. Read every diff; be ready to say the project was built with Claude Code.

## 13. Open items

- Object storage for receipts: filesystem volume vs MinIO (decide at M6).
- License (decide before going public).
- Whether `LISTEN/NOTIFY` is worth adding (decide after M3 benchmark).
- CI scope (M1.4): whether to build GitHub Actions at all now, and how much (see CLAIMS row 8, which lists lint, tests, govulncheck, secret scan; claim only what is built).
- Cleanup of expired `idempotency_keys` rows (they are ignored once expired but never deleted): periodic job, add with the queue in M3.
- Lock timeout on the idempotency claim: a concurrent duplicate waits on the unique index with no timeout if the first request stalls. Self-inflicted per user; add `lock_timeout` when hardening.
- Pagination for `GET /v1/groups` and the join-code list (currently `LIMIT 100`).
- Automated test for the `OIDC_DISCOVERY_URL` issuer/discovery split (implemented, untested; matters when the api runs in a container or the mobile client arrives).
- Automated PKCE login test against Dex (login is only exercised by `scripts/dev-token.sh`; CLAIMS row 6 stays `planned` until then).
- Tests skip silently without `TEST_DATABASE_URL` (34 skip with no database). Whatever runs tests in CI must set it and fail on skips.
- Rate limit is per api process; revisit if more than one api instance is ever run.
- First commit: fill the Commit column in CLAIMS.md for every row marked `built` (currently row 7).
