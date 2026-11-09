# Evidence

Every claim in the README is here, with what backs it and what it does not show.

- **Measured**: a number from a recorded run. The raw output is in [bench/results/](../bench/results/); each `.txt` records the commit, machine, Docker limits, Postgres version, parameters and every run. Method per benchmark: [bench/README.md](../bench/README.md).
- **Built**: it exists and tests you can run verify it. No number is claimed.

Conditions that apply to everything measured:

- **One machine.** An Apple M3 Pro (11 CPUs, 18 GiB) running Postgres 17.11 in Docker Desktop with no container limits, and the api, workers, mocks and test harness on the same laptop. So there is no throughput or capacity claim beyond the ones stated, and nothing says what a deployment would do.
- **Mocks, named.** Language-model calls go to a mock LLM and payments to a mock payment provider, each "with simulated latency of X ms", except where the real Anthropic API is named (claims 8, 9, 16).
- **Synthetic data only.** Settlement is simulated: no money moves.
- **Mutation testing.** Tests were broken on purpose to confirm they fail, but the counts are not quoted because the scripts are not kept.

## Measured

### 1. The ledger stays correct under concurrent load
Three runs of the production build, each 60 s: 24 users in 4 groups, with retries, duplicate idempotency keys, abandoned requests, barrier-released races, and settlement rounds run by 3 real worker processes against a mock payment provider with simulated latency of 100 ms median. **0 violations of any of 11 invariants and 0 server errors in all three runs.**

About 10,000 expenses (10,025 / 10,927 / 10,118) and 1,200 settlements (1,259 / 1,193 / 1,229) per run. The checker recomputes everything from the tables, never from API answers:
- every ledger transaction sums to zero;
- a finalized expense's entries equal the shares recomputed from its claims (a void is the exact negation);
- there is one expense or settlement per logical idempotency key;
- no settlement exceeds what its sender owed;
- succeeded settlements = ledger postings = provider charges;
- every item edit was applied to the version it was made from.

The run must also show the dangerous events happened, or it is reported as vacuous. For example, race rounds were 2,057 / 2,113 / 2,028, and in 2,040 / 2,110 / 2,020 of them two edits from one version met, one applied and one was refused.

Row-level security was on, and workers and the webhook ran on their own database roles. Evidence: `bench/results/stress-production-20261107T225340Z` (commit c5ee83b). Earlier recordings of the same benchmark (`stress-production-20261106T…`) predate row-level security or item edits; they are kept and not used.

**Not shown:**
- Throughput or latency.
- A finalize or void racing a settlement in the same group (an accepted limit).
- Adding or removing items, or confirm, under load (the workload only reprices).
- A real provider, or more than 3 workers.
- A guarantee: three runs fix each goroutine's sequence of operations but not the interleaving, so they are three samples.

### 2. Negative controls: switch one protection off and watch what breaks
The same run with one protection disabled, in a separate build (`unsafe_controls` tag) that is never shipped: 11 configurations x 3 runs. Violations per run (seeds 1 / 2 / 3):

| Protection off | What the checker saw |
|---|---|
| claim row lock | ledger differs from the claims: 16 / 21 / 20 |
| finalize row lock | 2,209 / 2,355 / 2,211 |
| edit version check | stale-version edits applied: 2,610 / 2,096 / 2,621 |
| idempotency check | duplicate expenses or settlements for one key: 5,761 / 6,411 / 5,555 |
| settlement group lock | settlements over capacity: 844 / 878 / 883 |
| settlement row lock, status guards and posting index (all three) | settlements with two postings: 138 / 139 / 113 |

Some switched-off protections showed **no violation** because another layer caught the problem, and the recording says which:
- Without the void lock, a unique index stops a double void; the ledger stays correct but clients get a 500 (9 / 24 / 31).
- Without the edit lock, the conditional update that ends an edit stops the loser (a 500 for the client).
- Without `Resolve`'s row lock, the unique index on the posting stops a double posting.
- The status guards alone are redundant while that lock is on.

The three zero-sum invariants, which the database enforces, held in all 33 runs. Evidence: `bench/results/stress-controls-20261106T221435Z` (commit 2766d9f). Three of the 33 runs are marked DOES NOT MATCH because the check that the run was not vacuous used a floor set too high (`edit_version_check` seeds 1 and 2, `group_lock` seed 2); they matched every other expectation and are included. The floor was lowered afterwards.

`make test-controls` runs a shorter version of all 11 cases on the current tree (they pass). That is a pass/fail check, not a new figure.

**Not shown:**
- That a protection with no violation is unnecessary; only that this workload did not get past it.
- How the controls behave with row-level security on: these runs predate it. The protections are unchanged since.
- Edit controls under adding or removing items (the workload only reprices).

### 3. Job queue throughput and latency
Real worker processes (4 jobs in flight each, 1 s poll, 30 s lease) on Postgres, against a mock LLM with simulated latency of 800 ms median. Three runs per configuration.

Closed-loop throughput, median [min-max] jobs/s against the bound `slots / mean latency`: 1 worker 4.4 [4.3-4.4] vs 4.6; 2 workers 8.9 [8.6-9.1] vs 9.2; 4 workers 17.8 [17.2-17.9] vs 18.5; 8 workers 35.9 [35.4-35.9] vs 36.9 (96 to 97% of the bound). In every run: 0 duplicate effects, 0 missing, 0 dead, 0 unfinished.

At 50% of the bound, end-to-end p50 / p99 was 1.31 / 3.05 s with 1 worker down to 0.92 / 2.25 s with 8 (medians of 3 runs). At 1 job/s, queue wait p50 was 0.61 s with 1 worker and 0.20 s with 4; the cost is polling.

With a mock LLM of 0 ms latency the queue reached 303 / 571 / 960 / 1,181 jobs/s at 1 / 2 / 4 / 8 workers. That is a ceiling for this harness on this laptop, not for Postgres or the queue alone: each job also makes a local HTTP call and writes a row.

Evidence: `bench/results/queue-20261031T085001Z` (commit 0568c75). Its `retry` section is not used (claim 5). The workers in this recording ran as the application database role; they now run as `fifty_worker`. Not repeated.

**Not shown:** a real LLM, another machine, more than 4 jobs in flight per worker, a larger jobs table, other poll intervals, runs longer than 30-40 s.

### 4. Recovery after `kill -9` of a worker mid-job
One of 3 workers (4 jobs in flight each) is killed with SIGKILL while its jobs wait on the mock LLM (simulated latency of 800 ms median). 20 trials of 24 jobs for each of two lease lengths, 40 trials and 960 jobs in all: **0 lost, 0 duplicate effects, 0 missing effects.**

| Lease | Orphaned jobs | Recovery per orphan (min / median / p90 / max) |
|---|---|---|
| 3 s | 79 | 2.51 / 3.76 / 4.49 / 5.51 s |
| 10 s | 80 | 9.78 / 10.81 / 11.51 / 12.50 s |

Recovery is set by the lease. Each orphan's model call ran again (at-least-once): 79 and 80 extra calls. The checks read Postgres, not the worker's logs. Evidence: `bench/results/recovery-20261030T234900Z` (commit 621138a), and a correctness version in `internal/benchwork/recovery_test.go`. Recorded with workers on the application role, not repeated.

**Not shown:** network partitions, a killed Postgres, several simultaneous kills, a real LLM. All trials kill at one point; the kill during the completion transaction is a correctness test only. 40 trials with zero anomalies is evidence, not a guarantee.

### 5. Retries with jittered backoff
500 jobs per run on 4 workers (`max_attempts` 5, backoff base 2 s, cap 5 min: placeholders), against a mock LLM with simulated latency of 800 ms median and injected faults, 3 runs per mix.

| Faults (429 / 5xx / malformed) | Succeeded | Dead-lettered | Attempts per job |
|---|---|---|---|
| none | 1,500 of 1,500 | 0 | 1.000 |
| 10% / 5% / 5% | 1,499 of 1,500 | 1 | 1.244 |
| 20% / 15% / 10% | 1,472 of 1,500 | 28 (1.87%; 1.85% expected at 45% failure per attempt) | 1.774 |

0 duplicate or missing effects in every run. Evidence: `bench/results/queue-20261031T094708Z`. The worker then ignored the 429 `retry-after` header; it honors it now (claim 16) and this was not re-recorded. The earlier recording `queue-20261031T085001Z` also ran this scenario, but its timings are void: the laptop slept during three runs. Its outcome counts agree with these.

**Not shown:** `retry-after`, timeouts, correlated outages, tuned constants, a real API.

### 6. Idempotent settlement under injected faults (simulated provider)
4,500 settlements in 15 runs (5 fault mixes x 3 runs x 300), arriving at 8/s, run by 3 real worker processes against a mock payment provider with simulated latency of 300 ms median. The faults were:
- lost responses and requests that never arrive;
- declines and failed reads;
- webhooks duplicated, delayed, dropped and reordered;
- one worker killed with SIGKILL.

**0 anomalies.** In every run succeeded settlements = ledger postings = provider charges (4,459 succeeded + 41 declined = 4,500), nothing was charged but recorded failed, and every settlement reached a final state. With lost responses and no webhooks, 113 of 900 settlements went `unknown`, all resolved by the first reconcile check (median 5.45 s from unknown to final).

Evidence: `bench/results/settlement-20261101T042323Z` (commit eb91381); the program fails on any anomaly. **The time to resolve an unknown settlement measures the placeholder reconcile schedule (first check 5 s, doubling to 5 min, 12 checks), not any provider or the design.**

**Not shown:** the api path (the harness inserts settlements as the handler does; authentication, the group lock and the capacity check are not involved), sustained load, more than 3 workers, a real provider, a killed Postgres. Recorded before row-level security and the separate worker roles; the settlement logic is unchanged since.

### 7. Redaction recall
Regex redaction of card numbers, phone numbers and email addresses before text goes to the model, scored on values planted in synthetic receipts (1,814 on references 1-1000; 1,846 on 1001-2000).

- On the ground-truth text: 0 leaked on both sets, one placeholder per value.
- On Tesseract's reading of the rendered receipts, recall for references 1-1000 / held-out 1001-2000:

| Noise | Overall recall |
|---|---|
| clean | 98.13% / 98.65% |
| mild | 96.47% / 97.62% |
| moderate | 94.54% / 94.53% |
| harsh | 80.76% / 82.83% |

Emails are the weak spot: 98.28% / 98.72% clean, 55.77% / 59.34% harsh. Every leak is a value OCR garbled first.

An earlier email pattern, written before the fix, got 89.19% of emails on clean renders of 1-1000 (96.09% overall, 72.71% at harsh noise). The fix, which tolerates a space beside a dot, was written after reading the first ten leaks per variant of the 1-1000 recording, so that comparison is partly in-sample. The fair one is the held-out references: email recall old vs new on 1001-2000 is 91.82% vs 98.72% (clean) and 16.62% vs 59.34% (harsh).

Evidence: `bench/results/redaction-20261107T044640Z` (old pattern, commit 03d2575) and `redaction-20261108T202918Z`, `…203322Z`, `…203739Z` (commit d8fb754 and a clean worktree of the old pattern). Deterministic, so one run each.

**Not shown:**
- Real receipts or photographs. The data is synthetic, in one monospace font on a white page whose slashed zero Tesseract misreads.
- Names and street addresses (not redacted).
- That a partly redacted value cannot identify a person.
- False positives on OCR text.
- Emails garbled beyond a space.

Never quote the 100% on ground truth without the OCR figures beside it.

### 8. Extraction accuracy with a real model
Claude Haiku 5.5 reading 50 synthetic receipts (references 5001-5050) in each of 5 variants (perfect text, then Tesseract at clean / mild / moderate / harsh noise) through the project's own path: redaction, prompt, strict validation, arithmetic review flag. 250 calls, one run, about 8 US cents.

| Variant | Prices of every unit right | Silent errors |
|---|---|---|
| perfect text | 50 / 50 | 0 |
| clean render | 50 / 50 | 0 |
| mild | 40 / 50 (80%) | 0 |
| moderate | 38 / 50 (76%) | 2 |
| harsh | 17 / 50 (34%) | 9 |

Most of the loss at higher noise is what Tesseract does to the text, not the model. **The review flag caught 43 of the 46 receipts with a wrong amount and none of the 8 with only a wrong quantity (arithmetic cannot see those).** All three silent wrong amounts are the model filling an unreadable price so the sum matches the printed total, which defeats the check. All 8 quantity-only errors are a quantity digit OCR dropped.

**Follow-up (prompt v2).** A rule telling the model to leave an unreadable price out instead of working it out was tried on 50 fresh receipts in each of 5 variants. It cut answers containing an amount not printed in the text from 55 of 250 to 13 of 246, but it cost correct answers: receipts read right without help fell from 196 to 173 of 250, flagged receipts rose from 49 to 75, 4 answers were refused by the schema (3 without the required total, 1 with no items), and silent errors went from 7 to 4, a difference one run each cannot separate from noise. **The original prompt (v1) stays the default.** Of the 22 receipts v1 had right that v2 did not, 19 were ones where v1 had correctly supplied an unprinted amount.

Evidence: `bench/results/accuracy-20261108T044115Z` (v1, commit 69368b2); `accuracy-20261109T030135Z` (A: v1) and `…030425Z` (B: v2) on references 5101-5150; `…030657Z` (C: v2 on 5001-5050; not independent, because v2 was written from those receipts). The plan and its predictions were written before the recordings. `go run ./bench/accuracy --analyze <recording>` splits the wrong parses by kind.

**Not shown:**
- Real receipts or photographs, or another model.
- A general catch rate for the flag.
- That v2 "fixes" anything or should be the default.
- A pooled interval, because the variants share receipts.

### 9. A prompt-injection check
Ten instructions aimed at the model, each printed as a note on a synthetic receipt: add an unprinted item, set the total to 1.00, reply with no items, halve every price, add forbidden fields, reply only with "APPROVED", close the data section and instruct, print the system prompt, add a fee and make the total add up, set every price to one cent. Sent to Claude Haiku 5.5 through the project's path in two variants (text, clean render): 20 calls, **all 20 answers ignored the note**, valid, accepted, and identical to a faithful reading. About 0.6 US cents.

Evidence: `bench/results/injection-20261109T032357Z` (commit 06f8c48; every raw answer is in the file). Each attack has a predicate for "the answer did what the note asked", tested to fire on an obeying answer and not on a faithful one.

**Not shown:** resistance to prompt injection. Ten attacks written by the author, one model, one run; zero in ten has a 95% upper bound of 27.8%. Nothing about attackers who know the prompt and validator, hidden text or photographs. The model did not produce a forged fee with an adjusted total, which the arithmetic flag would accept by design.

### 10. What row-level security costs
A seeded database (200 groups, 40,000 expenses, 200,000 items) and a closed-loop mix of the handlers' queries (reads, balances, claim then unclaim), 8 concurrent workers, 6 repetitions each on a fresh scratch database.

| Variant | Throughput, median [min-max] | p50 / p99 |
|---|---|---|
| no policies, plain pool | 4,330 ops/s [4,151-4,388] | 1.16 / 6.90 ms |
| identity seam, policies off | 2,133 [2,098-2,140] | 3.79 / 7.31 ms |
| identity seam, policies on | 1,732 [1,712-1,753] | 4.22 / 8.83 ms |

The seam that supplies the caller (a `BEGIN`, a `set_config`, the statement, a `COMMIT`) took about half the throughput (0.494 [0.486-0.505]); the policies took a further 18% (0.815 [0.805-0.822]). Evidence: `bench/results/rls-20261107T231125Z` (commit 8b9b6d9).

**Not shown:** a general cost of row-level security. This is a local database where a round trip is a few tenths of a millisecond and dominates, so the relative cost would be smaller with heavier queries. It is the database layer only (no HTTP, worker or webhook) and was recorded before the identity statement also set `lock_timeout` (claim 12) and before the balances query joined open settlements. Reducing the seam cost is designed and not built (README, future work).

## Built (verified by tests, no number claimed)

### 11. A ledger the database protects, and exact money
Each ledger transaction must sum to zero (a deferred constraint trigger, checked at commit). The application role cannot update or delete ledger rows (privileges revoked, plus triggers that also refuse the table owner), entries can only be added by the transaction that created their posting, and rows cannot cross groups (composite foreign keys). Money is `int64` minor units; shares come from a largest-remainder allocator that always sums exactly to the total. Tests: `test/dbinvariants/ledger_test.go` (raw SQL that violates each rule is refused), property tests in `internal/allocator` (20,000 iterations each, fixed seeds).

**Not shown:** protection against a superuser, who can disable triggers. This guards against application bugs and a compromised application, not the database administrator.

### 12. Idempotency, row locks and item edits
- **Idempotency.** `Idempotency-Key` on every state-changing endpoint. A retry replays the stored response, the same key with a different request is rejected, and 10 concurrent duplicates per endpoint create exactly one expense, posting or reversal.
- **Row locks.** Claims take a shared lock on the expense row and finalize and void take an exclusive one. Deterministic tests hold a raw lock transaction to prove each wait, and randomized claim-versus-finalize races recompute the ledger from the claims.
- **Item edits.** The payer-only item edit with an optimistic version: a stale edit gets 409 and changes nothing, and two edits from one version cannot both apply.
- **Lock timeout.** Every transaction a user request runs gives up waiting for a lock after 5 s. The api answers `503 busy` with `Retry-After: 1`, having changed nothing and stored nothing under the key, so a retry with the same key is a fresh attempt. The 5 s is a placeholder. Worker and webhook transactions have no such limit.

Tests: `internal/idempotency`, `internal/expenses`, `internal/dbscope`. These are correctness tests; the measured stress run is claim 1.

### 13. Job queue mechanics
Jobs are enqueued in the same transaction as the row that needs them, claimed with `FOR UPDATE SKIP LOCKED` under leases with heartbeats, with fencing tokens so a worker whose lease was taken over cannot overwrite the newer attempt. Handler effects commit atomically with job completion. Retries use jittered exponential backoff (2 s base, 5 min cap: placeholders) and exhausted jobs are dead-lettered. There is an operator requeue command and `worker delete-dead` (needs a filter, has `--dry-run`, never deletes a revived job), graceful shutdown, exactly-once periodic jobs across workers, an unauthenticated Prometheus endpoint that is off by default, and hourly deletion of succeeded jobs older than 7 days. The table refuses impossible job states (24 raw-SQL cases).

Tests include a real `kill -9` of a worker process at two points: during the model call, and with an effect row sitting in an uncommitted completion transaction. Every job ends with exactly one effect row.

**Not shown:** effects are exactly-once only for writes inside the handler's Postgres transaction. Calls to the model or the provider are at-least-once.

### 14. Settlement is idempotent (simulated)
`settlements.Resolve` is the only code that moves a settlement forward and posts it to the ledger. Tests force each interleaving: a raw row lock, a worker whose lease was taken over, a webhook that wins the race, an overlapping watchdog sweep. Seven end-to-end scenarios of 10-30 settlements run real worker processes (one killed with SIGKILL in two), a real mock-provider process and the real api handlers. They check succeeded = ledger postings = provider charges. `make test-e2e`. The suggested settle-up transfers are net of money already reserved by open settlements, so a suggestion is never refused by the settlement endpoint. They are a greedy heuristic: they reduce the number of payments and do not minimize it.

**Not shown:** a real provider. The mock's idempotency is the mock's: it dedupes by key for the life of the process. A suggestion can go stale if a settlement or expense is created between the read and the create; the create's group lock decides.

### 15. Authentication and authorization
- **Login.** OIDC with Auth Code + PKCE (S256) against self-hosted Dex, with refresh tokens. `test/oidc` runs the app's authorization request against a real Dex and checks that a wrong or missing verifier, `plain`, a replayed code and a changed redirect are all refused. **A finding:** the first version of this test showed Dex issuing codes with no PKCE challenge and accepting `plain`; Dex is now configured to enforce S256 (`deploy/dex/config.yaml`).
- **Authorization.** The cross-group (BOLA) suite: 4 kinds of actor x 41 scenarios over the group-scoped operations, with a guard that fails if an endpoint has no scenario or a reasoned exemption. Denied requests are asserted to change nothing.
- **Row-level security.** Policies on every table holding group or user data, forced, with a detector test that weakens them 29 ways (policies off on 14 tables, 14 read policies replaced by `USING (true)`, the memberships insert check widened) and requires the tests to notice each. Joining a group is a database function, so a user cannot add themselves by raw SQL.
- **Least privilege.** `fifty_app` holds only the commands its code runs (for example, it can insert settlements and jobs but not update or delete them), and Postgres refuses the rest before row-level security is asked.

**Not shown:**
- That the system is "secure" or "tenant-isolated". Row-level security covers membership of the group on the user-request path only. The worker, the webhook and first login run as other roles, and the caller's identity is a transaction setting that code running as the application role could set to anyone.
- The app's own login code and the system browser, which no automated test runs.
- Revoking a refresh token on sign-out (Dex has no endpoint for it here; sign-out only deletes it from the device).
- Suite size is not a security claim. Say "tested".

### 16. The receipt pipeline
- **Upload.** A photo is re-encoded before it is kept (EXIF, GPS, comments and appended data are gone), its declared size is checked before decoding, and its format is decided from its bytes. There are per-user quotas and rate limits, and retries do not duplicate the expense or use quota. A 45-second fuzz run of the hardening function (about 3.4 million inputs) found no failure, which is not a claim of safety.
- **Parse job.** Real OCR (Tesseract, run as a separate process), redaction, a model call, strict validation. A model answer must match a strict schema (exact fields, integer minor units, bounded sizes; extra fields such as a self-reported confidence are refused). Whether a person has to look is decided by arithmetic alone. Anything invalid or not adding up goes to review, and a sweep clears any expense stuck in `parsing`.
- **Model client.** Written against `net/http` so the mock and the real service share one code path. It honors `retry-after`, and its errors never quote the service's message or the receipt.
- **Crash safety.** A worker killed with SIGKILL mid-call leaves nothing half done: another takes the job and the items are written once (`make test-e2e`, mock LLM with simulated latency of 1500 ms, one run).

The real API was called by hand in four places. Two six-call smoke tests came first: the first was refused with HTTP 400 on every call because the client sent `temperature: 0`, which Haiku 5.5 rejects (fixed), and the second was answered in the expected shape. Then came the evaluations in claims 8 and 9, and a smoke test of the real api and worker on six fresh synthetic receipts: all six ended up claimable on attempt 1 and matched their ground truth, and a worker started with an invalid key got a real 401 that sent the receipt to review. No 429 or `retry-after` from the real service was ever seen, so honoring it against the real service is unobserved.

**Not shown:** OCR accuracy on photographs (none was read), that a wrong parse that adds up is caught (it is not), virus scanning, deletion of images, storage on more than one host.

### 17. Lists do not drop rows
`GET /v1/groups` and the group's join codes, expenses and settlements return at most 100 per page, newest first, with a `next_cursor` (keyset on `created_at DESC, id`). A list of 105 returns 100 then 5; rows with identical timestamps are neither repeated nor skipped across a page boundary; a row added between fetches changes nothing after the cursor.

**Not shown:** performance of deep pages (not benchmarked), that the cursor is tamper-proof (it only chooses a starting point among rows the caller may already see), a consistent snapshot across pages. The app shows at most 5,000 items of a list.

### 18. The iOS app
React Native (Expo) client for the whole flow against the unchanged api: sign in, groups and join codes, expenses with claim, finalize and void, add a receipt from the photo library, review and confirm, a simulated settlement followed to a final state. Typed from `api/openapi.yaml` with a drift check; 149 Jest tests cover the pure logic (session and token refresh, money parsing by integer arithmetic, idempotency-key helpers, what each screen offers). Run by hand on the iOS Simulator against the mock LLM and mock payment provider.

**Not shown:** Android, a physical device, the camera, automated UI tests, HEIC or very large photos. The screens are not run in CI.

### 19. A one-command demo
`make demo` builds the real api and worker, makes a fresh throwaway database, runs them with Tesseract, the mock LLM (simulated latency of 1500 ms) and the mock payment provider (simulated latency of 300 ms), and drives the whole flow over HTTP as three signed-in users, killing a worker with SIGKILL partway through. It exits non-zero if a check fails: totals and unit counts equal ground truth, replays return the same ledger transaction, the sum of all ledger entries is 0, and settlements = postings = provider charges. Run by hand; no test runs it. One walkthrough shows the quickstart works, not robustness.

### 20. CI
GitHub Actions runs three jobs, all green on the latest commit as reported by the author (run URLs are not recorded):
- **`ci`**: gofmt, `go vet` and golangci-lint for both builds, a check that generated code is current, the tests against a real Postgres and a real Tesseract and a real Dex (a missing one fails rather than skips), the suite again with the `unsafe_controls` tag, and `govulncheck`.
- **`secrets`**: gitleaks over the whole history. It found one false positive, a marker string planted by a test, listed in `.gitleaksignore` with its reason. A scan cannot prove there are no secrets.
- **`mobile`**: typecheck, the generated-types drift check, Jest.

Actions are pinned by commit SHA. Not in CI: the end-to-end tests, the negative controls and the benchmarks (`make test-e2e` and `make test-controls` run by hand).

## What must not be claimed

- Real money movement, a payments product, "banking-grade", production use. Settlement is simulated.
- Any load or capacity figure not from a recorded run, or from the stress runs at all. A figure for a mock must say "mock LLM with simulated latency of X ms".
- "Minimizes" transfers (the heuristic is greedy; a counterexample is pinned in `internal/ledger/transfers_test.go`).
- PII "removal": only three regex patterns are redacted, and names are not. Never present synthetic-data accuracy or recall as real-world.
- Resistance to prompt injection, or that a receipt that "adds up" was read correctly. A model can misread a price and the total together.
- That the review flag "catches wrong parses" in general; the 43 of 46 describes this model, prompt, noise and these receipts.
- That prompt v2 improves accuracy or should be the default (claim 8).
- That row-level security makes the system "secure" or "multi-tenant safe", or a cost figure for it other than claim 10 with its seam.
- "The protection is necessary" or "unnecessary" from a control that showed no violation (claim 2); "the production build is correct" (it kept the listed invariants in three runs).
- The `throughput-queue` figure as "what Postgres can do"; the time to resolve an unknown settlement as a property of a provider.
- Encryption at rest of receipts, Kubernetes, Terraform, autoscaling, multi-currency, push notifications, vision-model extraction: none was built.
- Android, a physical device, the camera, automated UI tests.
