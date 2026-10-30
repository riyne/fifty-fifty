# Fifty Fifty: design

Status: approved decisions as of 2026-10-30 (M0-M2 done; M3 in progress, 3.1-3.3 done). Update this file when a decision changes; don't let it drift from the code.

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
| M2 scope decisions (2026-10-29) | (1) M2 accepts only per-unit items; expanding "3 x beer" into units is a pure helper built in M6 with the receipt parser, its only caller. (2) The expense creator is the payer; the `payer` column exists but "payer != creator" is not supported (multi-payer is cut). (3) A member claims only for themselves; the only way a third party is assigned is "split the rest evenly" at finalize, which writes real claim rows in the finalize transaction so "ledger = recomputed claims" holds. (4) Finalize: payer only. Void: payer or group owner. (5) Item edits are allowed in `needs_review` and `claiming`; an edit deletes the claims on the edited items (they were made at the old price). (6) A void after a later settlement (M4) can leave a pair overpaid so balances flip sign; that is correct ledger behavior, not a bug. Prices and tax/tip are non-negative: no negative discount lines (enter a discount as reduced item prices) |
| Allocator (M2.1) | `internal/allocator`, pure Go, stdlib only. `Apportion` is largest-remainder over integer weights using 128-bit multiply/divide (`math/bits`), so `total*weight` cannot overflow. Ties go to the lowest member id (bytewise uuid order); output is sorted by id and independent of input order. `Allocate` splits each item evenly among its claimants, then prorates tax and tip by claimed subtotal. Known property: with equal weights each item's leftover cent goes to the lowest-id claimant, so across k shared items one member can be up to k cents over exact (per item, under 1 cent). A rotating tie-break would spread this but deviates from the "tie-break by member id" rule above; not adopted |
| DB roles (M2.2, migration 00004) | Two Postgres roles. Migrations run as the owner/superuser `fifty` (`MIGRATE_DATABASE_URL`). The api and all integration tests run as `fifty_app` (`DATABASE_URL`, `TEST_DATABASE_URL`): login, not superuser, not owner, no DDL, no BYPASSRLS. Before this the app connected as the superuser, which bypasses GRANT/REVOKE, so "app role has no UPDATE/DELETE on ledger tables" was unenforceable. `fifty_app` gets SELECT/INSERT/UPDATE/DELETE on everything via default privileges, then ledger tables and `audit_events` have UPDATE/DELETE revoked. The dev password is in the migration (throwaway, same as compose); a real deployment must `ALTER ROLE fifty_app PASSWORD` out of band. `TEST_OWNER_DATABASE_URL` exists only for tests that must show a protection holds against the owner. A test asserts the test connection really is the unprivileged role |
| Ledger schema (M2.2, migration 00005) | Net signed entry per member per ledger transaction (unique on transaction+member, zero rows not stored; payer who also claimed gets one net entry). No stored expense total (derived from items + tax + tip). Kinds `expense` and `void` only; `settlement` is added in M4 together with its link column. Composite foreign keys make cross-group rows impossible: payer, claimant and ledger-entry member must be members of the row's group; a claim's item, expense and group must agree; a posting's expense must be in its group; a void must reverse a posting of the same expense. One `expense` posting and one `void` per expense (partial unique indexes), so a duplicated finalize or void cannot post twice. Zero-sum is a deferred constraint trigger (checked at COMMIT, `SUM` is numeric so it cannot overflow). Entries can only be inserted by the database transaction that created their header (`txid xid8` column + trigger), so a committed posting can never be extended, even with a balanced pair. Append-only: UPDATE/DELETE/TRUNCATE revoked from the app role, plus triggers that refuse them for everyone including the owner and superusers (a superuser can still disable triggers; the claim is protection against bugs and a compromised app, not against the DBA). Not enforced by the database, on purpose: that no claim changes after finalize. A status-check trigger would not close the claim-vs-finalize race (it reads committed state), so that stays with the row locks and the stress test |
| Expenses API (M2.3) | `POST /v1/groups/{id}/expenses` (any member; creator is the payer; requires `Idempotency-Key`), `GET /v1/groups/{id}/expenses` (newest first, at most 100), `GET /v1/groups/{id}/expenses/{expenseId}`. Created expenses start in `claiming` with all items in one transaction. The client never sends a total: `total_minor` = sum of items + tax + tip, derived on every read. Limits: 1-100 items, each amount an integer 0..100,000,000 minor units (1,000,000.00), text 1-200 chars trimmed; `price_minor` is required (missing is not 0); fractional/float JSON numbers are rejected; tax and tip must be 0 if every item is free (nobody to prorate them to, finalize could never succeed). Order of checks on create: membership (404) -> Idempotency-Key (400) -> body (400) -> idempotent transaction, so a non-member learns nothing, not even that their body was malformed. Idempotency fingerprint is the group id plus the validated, normalized request (trimmed text, defaulted tax/tip), so equivalent requests replay and the same key in another group is a 422, not a replay. Reads: one statement joins `memberships` and filters on the path's group; unknown, foreign-group and malformed expense ids and non-members all give the same 404 body (`expense not found`). A user in two groups can read an expense only through its own group's path. `GET` detail reads the expense and its items in one read-only REPEATABLE READ snapshot. Items carry `claimed_by` (sorted member ids; empty array, never null). No audit event on create (DESIGN lists finalize/void/settle/membership/join-code only) |
| Claims API (M2.4) | `PUT` / `DELETE /v1/groups/{id}/expenses/{expenseId}/items/{itemId}/claim`: a member claims or unclaims for themselves only (no member parameter). Naturally idempotent (re-claim is a 200 no-op, unclaiming a claim you do not hold is a 204 no-op), so no `Idempotency-Key`. One transaction: the first statement takes `FOR SHARE` on the expense row, authorizes (membership join, path-group filter) and returns the status; any status other than `claiming` (including `parsing` and `needs_review`) is 409 `expense_not_claimable`; then the item is looked up scoped to the expense (404 `item not found`), then the write. `FOR SHARE` is deliberate: compatible with itself (claimers never queue behind each other), conflicts with `FOR UPDATE` (finalize, void) and with `FOR NO KEY UPDATE` (a plain UPDATE, i.e. item edits), which `FOR KEY SHARE` would not. The claim's own FK checks take `FOR KEY SHARE` on the expense row, which also waits for `FOR UPDATE` but is not what protects the status check. 404 bodies: non-member, unknown, foreign-group and malformed expense ids are all `expense not found`; a malformed item id is looked up after the membership check so it cannot tell a non-member anything. Tested without finalize by a raw transaction standing in for it: claimers share the lock, a claim waits for a `FOR UPDATE` holder and is then rejected with 409, and a randomized race (8 users claiming/unclaiming against a stand-in finalizer, 15 rounds) shows the claim set the finalizer read is exactly the set that remains |
| Finalize (M2.5) | `POST /v1/groups/{id}/expenses/{expenseId}/finalize`, payer only, `Idempotency-Key` required, optional body `{"split_rest_among": [member ids]}`. Order of checks: path ids, then authorization with no write lock (404 non-member, 403 member who is not the payer; the group owner has no special rights here), then key (400), body (400) and split-list membership (400) before the key is claimed, then one idempotent transaction. Inside it: `FOR UPDATE` on the expense row (waits for in-flight claims, blocks new ones, so the claim set read next is stable until commit); status must be `claiming` (else 409 `invalid_state`); items with no claimant give 409 `items_unclaimed` unless `split_rest_among` is given, in which case real claim rows are written for them in the same transaction; `allocator.Allocate`; `ledger.ExpenseEntries`; ledger header + entries; expense marked finalized (conditional on `claiming`); `expense.finalized` audit event (`expense_id`, `ledger_transaction_id`, `total_minor`, `split_rest_items`). Postings are net per member: each non-payer claimant is debited their total, the payer is credited the sum of those (so a payer who also claimed is credited only the difference), zero entries are omitted, and a payer who ate everything posts a header with no entries. The response is the finalized expense plus `ledger_transaction_id` and `shares` (per-member subtotal/tax/tip/total, payer's own share included). Because every 4xx from inside the transaction is stored and replayed (Stripe model), a client that gets 409 `items_unclaimed` and then fixes it must retry with a NEW key; the OpenAPI text says so. The lock query authorizes by `payer_id` in the path's group and has no membership join because the foreign key on `expenses (group_id, payer_id)` already makes a payer a member (a join there was provably redundant and was removed after a mutation check found no test could tell it apart). Core invariant, tested by recomputing from the claims table without reading the response: the ledger contains exactly the entries the claims imply and sums to zero (`assertLedgerMatchesClaims`) |
| Balances and suggested transfers (M2.6) | `GET /v1/groups/{id}/balances` (any member): every current member, in join order, with `balance_minor` = SUM of their ledger entries in that group (positive: the group owes them; negative: they owe the group; 0 if none), the group's currency, and `suggested_transfers` (`from_member_id` pays `to_member_id` `amount_minor`, derived at read time, nothing is posted). Balances and transfers come from a single statement, so a response is one snapshot: it always sums to zero and applying its transfers always gives all zeros, even while expenses are being finalized (tested by polling during 60 concurrent finalizes; replacing it with two reads is caught). The query authorizes in its own statement (join on the caller's membership) and the handler also answers 404 for non-members. Unfinalized expenses count for nothing. Simplification is `ledger.SuggestTransfers`, a pure greedy: repeatedly the member owed the most is paid by the member who owes the most, for the smaller amount, ties to the lowest member id, so at most n-1 transfers for n nonzero balances and independent of input order. It **reduces** transfers and does not minimize them (the exact optimum is NP-hard: split members into as many zero-sum groups as possible): balances -8, -7, +7, +2, +6 need 3 transfers, greedy gives 4 (pinned in a test; on random small inputs it was worse than optimal in 4 of 3000 cases). Settlements (M4) will post to the same ledger, so this endpoint will reflect them without changes |
| Void (M2.7) | `POST /v1/groups/{id}/expenses/{expenseId}/void`, payer **or group owner**, `Idempotency-Key` required, no body (`{}` accepted, other fields rejected). Same shape as finalize: path ids, authorization without a write lock (404 non-member, 403 member who is neither payer nor owner; the lock query repeats the payer-or-owner test as a backstop and a test calls it directly), then key (400), then one idempotent transaction: `FOR UPDATE` on the expense row (exclusive; tested directly, because two real voids collide only by luck), status must be `finalized` (else 409 `invalid_state`, including an already-void expense), a `void` ledger transaction with `reverses_id` pointing at the finalize posting and `created_by` the caller, the original entries copied with their sign flipped by a single `INSERT ... SELECT` (so the reversal is the exact negation by construction), the expense set to `void` (conditional on `finalized`, `voided_at` set, `finalized_at` kept), and an `expense.voided` audit event (`expense_id`, `ledger_transaction_id`, `reverses_ledger_transaction_id`, `reversed_entries`, `voided_as`: `payer` or `owner`; the actor is the caller). Only a finalized expense can be voided: a claiming expense has nothing on the ledger, the schema requires a void expense to have been finalized, and cancelling an unfinalized expense is not in the approved scope. Nothing on the ledger is updated or deleted, claims are left untouched as the record of who had what, and a void expense accepts no claims, unclaims, finalize or second void. Net effect, tested: group balances after a void equal balances as if the expense had never been finalized, whatever else happened in between. The response is the void expense plus `ledger_transaction_id` (the reversal) and `reversed_ledger_transaction_id`. A void after a later settlement (M4) can flip a pair's sign; correct, see the M2 decisions |
| M2 closed early; 2.8 and 2.9 deferred (2026-10-29) | Ryan delegated this call after M2.7. Checked against the code and docs first. **2.9 (negative-control seams) goes into M5**: M5's scope already lists negative controls, and the seam is a real design problem that needs the stress harness in view. The locks are literal `FOR SHARE` / `FOR UPDATE` text in sqlc-generated `const` query strings, so they cannot be toggled at runtime (forbidden anyway), and putting "unsafe" alternate queries in sqlc output would compile unsafe SQL into the production binary, violating "only under the `unsafe_controls` tag". The design therefore needs the unsafe variants in a build-tagged hand-written file (or a separate sqlc config), with a build-tagged selector at the three call sites. Nothing is lost meanwhile: every lock and check is already shown load-bearing by deterministic tests that fail when it is removed (mutation runs through M2.7). **2.8 (item edits) goes into M6**: its only consumer is the `needs_review` path; the section 10 stress mix (create/claim/finalize/void/settle) does not use it; nothing reads `version`. Building it earlier would write the riskiest remaining concurrency code without its real user. Cost, accepted and recorded: until M6 a wrong price on a `claiming` expense cannot be corrected, only worked around by finalizing, voiding and recreating (which leaves a posting and its reversal in the history). The README must say so. Verification before closing: whole suite clean under `-race` and over 3 repeated runs |
| Queue schema and enqueue (M3.1, migration 00006) | One `jobs` table, shape per section 6. The database refuses every column combination the state machine cannot produce: the lease columns (`lease_token`, `lease_expires_at`) are non-null exactly while `state = 'running'`; `finished_at` is non-null exactly in `succeeded`/`dead`; `attempts <= max_attempts` (a job never runs more than `max_attempts` times, so the claim must not take an expired lease on a job that has used its last attempt; M3.2/3.4 move it to `dead` instead); payload must be a JSON object. A claim increments `attempts`, so while running it is the current attempt's number. `max_attempts` has no database default: `queue.DefaultMaxAttempts` (5) is the one place the default lives. The two partial indexes this migration created were replaced in M3.2 (next row). `queue.Enqueue` takes a `pgx.Tx`, not a pool, so a job cannot be enqueued outside the transaction of the row that needs it (no dual write); `run_at` defaults to the database clock. Tested: commit stores a queued job, rollback leaves none, invalid jobs are refused before any write, 10 concurrent duplicate idempotent requests enqueue exactly one job (the pattern M4 and M6 use), and `test/dbinvariants/jobs_test.go` runs 24 raw-SQL cases (5 must be accepted so a harness typo cannot make the refusals pass for the wrong reason) |
| Claim and leases (M3.2, migration 00007) | `queue.ClaimJobs(pool, {Kinds, Limit, Lease})` is one autocommitted statement: a `MATERIALIZED` CTE selects the oldest eligible rows with `FOR UPDATE SKIP LOCKED`, and the `UPDATE` sets `state = running`, `attempts + 1`, a **new** `lease_token` (`gen_random_uuid()`, so every claim is a new fencing token) and the lease expiry, all on the database clock. Concurrent claimers pass over each other's rows instead of waiting. A worker names the kinds it has handlers for and there is no "any kind". It takes the pool, not a transaction, so the row locks last one statement; what keeps other workers off a running job is the lease, not a lock. **Eligibility is one timestamp:** while a job is running, `run_at` equals `lease_expires_at` (a CHECK keeps them equal), so "claimable" is `state IN (queued, running) AND run_at <= now() AND attempts < max_attempts`, covering both a due queued job and a running job whose lease expired, served by one partial index `jobs_claim_idx (run_at)`. Reason, measured on 200k rows (100k finished, 50k due, 50k delayed; temp table, Postgres 17.11, one run, not a benchmark): the 00006 design (two partial indexes, `OR`) made the planner read and sort every due row, 11.9 ms per claim and growing with the backlog; the single-timestamp form is an ordered index scan that stops after `LIMIT` rows, 0.03 ms. Cost accepted: each heartbeat changes an indexed column, so it cannot be a HOT update. Jobs of kinds nobody claims sit in the index and are skipped by the filter, so a large backlog of an unhandled kind slows every claim; fine while all kinds have a handler. Within equal `run_at` the order is unspecified. A job whose last attempt's lease expires is not claimable (`attempts` may not exceed `max_attempts`); `queue.ReapExhausted` moves it to `dead` with `last_error = 'lease expired on attempt N of N'` (the worker loop calls it periodically, M3.5; it is safe against a worker that completes late, because both are conditional UPDATEs that serialize on the row). Tested: oldest-first and limit, takeover of an expired lease (attempt 2, different token), every ineligible kind of row skipped, rows locked by another transaction skipped without waiting (deterministic: a raw `FOR UPDATE` holder, 3 s bound), 8 workers draining 300 jobs claim each exactly once (stable over 25 runs under `-race`), reaper leaves live leases and jobs with an attempt left alone. Mutation checks (throwaway script, not in the repo) confirmed failing tests for: no row lock, `FOR UPDATE` without `SKIP LOCKED`, ignoring `attempts < max`, `run_at`, the kind filter, `LIMIT`, running-job takeover, token reuse, attempt count, and each reaper condition. Two survivors, both equivalent here: removing `ORDER BY` (the planner returns index order anyway; the contract is the `ORDER BY`, and no test can force a different plan) and `NOT MATERIALIZED` (Postgres 17 does not inline a CTE that takes row locks; `MATERIALIZED` is kept so correctness does not rest on that) |
| Fencing, completion and failure (M3.3) | A worker's only writes to a job it holds are `Heartbeat`, `Complete` and `Fail`, each one conditional UPDATE on `id` and `lease_token`. A stale worker (its lease expired and another claim wrote a new token) matches no row and gets `ErrLeaseLost`, with nothing changed. There is deliberately no `state = 'running'` in them: the 00006 CHECK makes `lease_token` non-null exactly while running, so a matching token already implies running (a mutation check found no test could tell the extra condition apart, so it was removed; if that CHECK is ever relaxed it must come back). **`Complete(tx)` must be the last statement of the transaction that makes the handler's effects**, and `CompleteWith(pool, claimed, fn)` is that shape: `fn` writes, then `Complete`, then commit. If `fn` errors, everything rolls back and the job stays running under the same lease (the caller then calls `Fail`); if the lease was lost, everything rolls back and `ErrLeaseLost` is returned, so a stale worker's effects never exist. Why last: the completion's row lock stops any claim (`SKIP LOCKED`) taking the job until commit, and it is the fence check at the latest possible point. Completing first would hold the job's row lock through the whole handler and block heartbeats (a mutation that did this made a test hang). `Fail` takes the pool and runs after the attempt's transaction rolled back: the job goes back to `queued` at `RetryAt` (default now) or to `dead` when `attempts = max_attempts`, with `last_error` (cut to 2000 bytes on a character boundary). The backoff policy that chooses `RetryAt` is M3.4. `Heartbeat` extends `lease_expires_at` and `run_at` together; it succeeds on a lease that expired but was not taken over (token still matches, nobody else holds the job). Exactly-once covers only writes inside the handler's Postgres transaction; anything the handler does elsewhere (the LLM or provider call) is at-least-once. Tested: stale worker's heartbeat, complete and fail all lose and leave the newer attempt byte-identical; nothing works on a finished or dead job with an old token; forged and cross-job tokens; effects and completion commit together; a failing handler discards its effects; a worker paused mid-job (writes its effect, stalls past its lease, another worker takes over and finishes, the first resumes) leaves exactly one committed effect; a late-but-not-replaced worker that has run `Complete` cannot be overtaken before it commits; 40 rounds of concurrent `Complete` vs `Fail` have exactly one winner. Mutation checks (throwaway script) confirmed failing tests for: each of the three statements ignoring the token, `Fail` never dead-lettering, keeping the lease, ignoring `RetryAt` or not recording the error, heartbeat not moving `run_at`, `Complete` treating zero rows as success, `CompleteWith` ignoring the handler error or committing after a lost lease or completing first, `Fail`/`Heartbeat` swallowing lease loss, and truncation splitting a character |
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

Repo layout. Built (as of M3.3, 2026-10-30; seven migrations, 00001-00007):

    api/openapi.yaml                 the spec; test/bola reads it to check authz coverage
    cmd/api/                         HTTP server entrypoint
    internal/allocator/              pure share math: largest-remainder apportion, per-expense allocate
    internal/auth/                   OIDC verifier + middleware + first-login provisioning
    internal/auth/authtest/          in-process fake issuer (tests only; nothing in prod imports it)
    internal/db/                     sqlc output (committed) + queries/*.sql (source)
    internal/expenses/               expense create/list/get handlers
    internal/groups/                 group + join-code handlers
    internal/balances/               group balances + suggested transfers handler
    internal/ledger/                 pure bookkeeping: expense shares -> ledger entries; greedy transfer suggestions
    internal/httpapi/                root handler, health, /v1 wiring
    internal/httpx/                  JSON read/write helpers
    internal/idempotency/            idempotency-key helper (claim-in-transaction)
    internal/joincode/               join code generate/normalize/hash
    internal/queue/                  job queue (M3.1 Enqueue, M3.2 ClaimJobs and ReapExhausted, M3.3 Heartbeat, Complete, CompleteWith, Fail; backoff and requeue follow in M3.4)
    internal/ratelimit/              in-memory per-user token bucket
    internal/teststack/              whole api in-process for integration tests
    internal/testutil/               test DB pool (TEST_DATABASE_URL), unique subjects
    migrations/                      goose SQL migrations (sqlc reads these as its schema)
    deploy/docker-compose.yml        Postgres 17 + Dex; deploy/dex/ (config baked into image)
    scripts/dev-token.sh             real Auth Code + PKCE login against Dex, prints an ID token
    tools/go.mod                     separate module pinning goose and sqlc
    sqlc.yaml  Makefile
    test/bola/                       BOLA suite
    test/dbinvariants/               raw-SQL tests of what the schema itself refuses (ledger, cross-group rows)

Planned, not built yet:

    cmd/{worker,mockllm,mockpay,stress}/
    internal/{queue,receipts,settlements,...}/
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
- Item edits (payer only) use the `version` column as an optimistic lock (conditional update; zero rows -> 409). **Not built yet: arrives in M6 with the `needs_review` path.** The `version` column exists and is returned by the API; it is always 1 until then.
- Settlement creation: validated under a per-group lock (lock the group row). Available amount = what the payer owes, bounded by what the payee is owed, minus the sum of pending settlements. Success posting does not re-validate (it records a payment that already happened).
- DB-enforced invariants: unique constraints, deferred constraint that each ledger transaction sums to zero, app role has no UPDATE/DELETE on ledger tables (plus a trigger and a test).
- Considered, not adopted: bumping `version` on every claim so finalize is a compare-and-swap on the reviewed state. It would serialize claimers on one row. Revisit if UX needs it.

Idempotency: required `Idempotency-Key` header on create-expense, finalize, void, create-settlement, join-group.
- Same key + same fingerprint: replay the stored response (a concurrent duplicate waits on the unique index and then replays).
- Same key + different fingerprint: 422.
- Keys expire after 24 h.
- Implemented (M1.3) as `internal/idempotency`: the key row is claimed in the same transaction as the operation and completed before commit, so effects, stored response and key commit or roll back together (no "in progress" state). Every response below 500 is stored and replayed, including business-rule 4xx (Stripe model: a key's first outcome is final). 5xx and errors roll back and are not stored, since the transaction is atomic and a retry should be able to succeed. Validation errors, 401 and 429 are rejected before the key is claimed. Handlers that return a non-2xx result must not have written anything they don't want committed.

Negative controls: the version check, row locks, and idempotency check can each be disabled only in a test binary built with the `unsafe_controls` tag. There is no runtime flag in the production binary. **Built in M5, not M2** (see the decision log). The M5 benchmark can only report controls for protections that exist by then: the claim lock, the finalize/void lock and the idempotency check. The version-check control arrives with item edits in M6 and is NOT part of the M5 numbers; CLAIMS rows must say exactly which protections were disabled.

## 6. Job queue

Single `jobs` table: kind, payload, state (`queued` | `running` | `succeeded` | `dead`), attempts, max_attempts, run_at, lease_token, lease_expires_at, last_error.

- Claim: one statement selects eligible rows (`queued` and due, or `running` with an expired lease) with `FOR UPDATE SKIP LOCKED`, sets a fresh lease token and expiry, increments attempts. While a job runs, `run_at` is its lease expiry, so eligibility is the single condition `run_at <= now()` (M3.2 decision log).
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
- Quantity lines (e.g. 3 x beer) are expanded into per-unit items so units can be claimed individually (remainders by the same allocator). Settled at M2: the expense API only accepts per-unit items; the expansion is a pure helper built here in M6.
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
| M1 | Go module, compose (Postgres, Dex), migrations, sqlc, OIDC middleware, users/groups/join codes, CI skeleton. **M1.1 (module, compose, migrations, health), M1.2 (users/groups/memberships, sqlc, OIDC, fake issuer) and M1.3 (join codes, idempotency, rate limit, audit, first BOLA suite) done (commit e611bb6). M1.4 (CI skeleton, GitHub Actions only) is deferred to just before M5 / going public, by Ryan's decision on 2026-10-20; M1 is otherwise complete** | ~20 |
| M2 | Ledger core: expenses, claims, finalize, void, allocator, balances, simplification. Sub-steps (approved 2026-10-29): **2.1 allocator (done)**; **2.2 schema + DB-enforced invariants, incl. the app/owner role split (done)**; **2.3 create/read expenses (done)**; **2.4 claim/unclaim (done)**; **2.5 finalize (+ split the rest) (done)**; **2.6 balances + simplification (done)**; **2.7 void (done)**; **M2 is closed at 2.7.** The two remaining sub-steps were deferred on 2026-10-29 (Ryan delegated the call; reasons in the decision log): 2.8 item edits moves to M6, with the receipt `needs_review` path that is its only consumer; 2.9 `unsafe_controls` seams moves into M5, whose scope already says "negative controls" | ~25 |
| M3 | Queue: leases, fencing, backoff, DLQ, kill-a-worker recovery test and benchmark. Sub-steps (approved 2026-10-30): **3.1 jobs table + transactional enqueue (done)**; **3.2 claim with leases (done)**; **3.3 fencing: heartbeat, complete, fail (done)**; 3.4 backoff with jitter (the policy that picks `RetryAt`), operator requeue command, end-to-end retry-to-dead tests; 3.5 worker runtime + `idempotency.cleanup` job; 3.6 metrics endpoint; 3.7 `cmd/mockllm`; 3.8 recovery test and benchmarks 2-4 (may split into 3.8a recovery, 3.8b throughput/retry) | ~20 |
| M4 | Settlement: idempotency, mock provider, async execution, reconciliation | ~20 |
| M5 | Stress test, negative controls (the `unsafe_controls` seams, deferred from M2.9; design note in the decision log), BOLA suite, benchmark write-up. **Resume-usable numbers (~5-7 weeks at 15 h/wk)** | ~20 |
| M6 | Receipt pipeline: generator, Tesseract, redaction, mock + Anthropic extractor, review flag, **and item edits** (payer-only, `version` optimistic lock, edit clears claims on the edited items; deferred from M2.8 because `needs_review` is its only user). Estimate not yet re-baselined for the added work | ~25 |
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
- CI (M1.4), deferred to just before M5 / going public. Build the minimal version then: tests against a Postgres service with `TEST_DATABASE_URL` set and skips treated as failures, `go vet`, gofmt, `govulncheck`, and a check that `make generate` leaves no diff. golangci-lint (not installed) and secret scanning after that. CLAIMS row 8 stays `planned` and must list only what is actually built. Until then `make test` is the only gate and nothing enforces it.
- Cleanup of expired `idempotency_keys` rows (they are ignored once expired but never deleted): periodic job, add with the queue in M3.
- Lock timeouts: a concurrent duplicate waits on the idempotency unique index with no timeout if the first request stalls (self-inflicted per user), and a claim waits without a bound for a stalled finalize/void/edit holding `FOR UPDATE` on the expense row (the request ends when the client disconnects or the server write timeout hits). Add `lock_timeout` when hardening.
- `GET /v1/groups/{id}/balances` is O(ledger entries in the group) per request (a SUM over an index-only scan). Fine at the sizes this project reaches; if the stress test shows it matters, add a cached running balance, but then the zero-sum and derived-by-SUM invariants must be re-proved.
- Pagination for `GET /v1/groups`, the join-code list and the expense list (all currently `LIMIT 100`; an 101st expense is simply not returned).
- Automated test for the `OIDC_DISCOVERY_URL` issuer/discovery split (implemented, untested; matters when the api runs in a container or the mobile client arrives).
- Automated PKCE login test against Dex (login is only exercised by `scripts/dev-token.sh`; CLAIMS row 6 stays `planned` until then).
- Tests skip silently without `TEST_DATABASE_URL` (about 100 tests and subtests skip with no database, as of the end of M2). Tests that need the owner role also skip without `TEST_OWNER_DATABASE_URL`. Whatever runs tests in CI must set both and fail on skips. Running `go test` by hand does not get them from the Makefile: export them in the same shell command, then confirm with `-v` that tests ran and did not skip.
- Rate limit is per api process; revisit if more than one api instance is ever run.
- Migration 00004's Down drops the cluster-wide role `fifty_app`; that fails if another database in the same Postgres cluster still has privileges for it. Fine for the single-database dev stack; revisit if that changes. Roles are cluster-global, so tests or scripts that `ALTER ROLE fifty_app` leak across databases (this bit the mutation checks in M2.2 once).
- The integration-test database is shared and never cleaned, and `httptest` issuer URLs are random local ports that get reused, so any test that counts rows by issuer URL must compare against a baseline taken after the stack starts (two tests in `internal/httpapi` asserted absolute counts and began failing once the database had accumulated thousands of users; fixed in M2.5). New tests should create their own identities and never assume an empty table.
- Until item edits exist (M6), a typo in an item price on a `claiming` expense cannot be fixed; the workaround is finalize (with `split_rest_among`), void, then recreate. State this in the README.
- Negative-control seam design for M5 (see the decision log): unsafe query variants must live in a build-tagged hand-written file, never in sqlc output; decide the selector mechanism and how anomaly counts are reported when the stress harness is built.
- Worker role: the worker (M3) will need its own role if/when the RLS stretch (M7) lands; for now it would share `fifty_app`.
- sqlc maps the `ledger_transactions.txid xid8` column to `pgtype.Uint64`; no query selects it. If one ever needs to, check the scan.
