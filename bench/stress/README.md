# Ledger stress and negative controls

Drives the real api with many users at once and then checks, from the database, that nothing the system promises was broken. Then repeats the run with one protection switched off at a time, to show the checker can see the damage. Results: [claims 1 and 2](../../docs/EVIDENCE.md).

    make bench-stress          # both builds, about 51 minutes: caffeinate -ims make bench-stress
    go run ./bench/stress      # the production build alone (export MIGRATE_DATABASE_URL and DATABASE_URL as the Makefile does; the program refuses to start without them)

`make bench-stress` runs two builds, one after the other, each writing its own result files:
- `go run ./bench/stress` is the **production build**, nothing disabled (`bench/results/stress-production-*`).
- `go run -tags unsafe_controls ./bench/stress` is the **negative-control build**, one protection off at a time (`stress-controls-*`). The workers are built with the same tag. This is the only build in which a lock or a guard can be removed (see `internal/controls`), and it must never be shipped.

Flags: `--configs 1,3` runs some configurations; `--runs` and `--duration` shrink it for a smoke run (needs `--allow-dirty`, never committed). Settlement is simulated: every result says "mock payment provider with simulated latency of 100 ms".

## What one run does

Creates a scratch database (every migration applied, dropped afterwards). Starts the real api in the harness process (the same verifier, middleware and handlers as `cmd/api`, tokens from an in-process issuer, the payment webhook mounted), a real `mockpay` process and 3 real `worker` processes (4 jobs in flight each, 1 s poll, 5 s lease). For 60 seconds:

- **24 users in 4 groups**, each a loop choosing create (18%), claim or unclaim (41%), edit (6%), finalize (17%, 40% of them with `split_rest_among`), void (8%) or a balances read (10%). An edit reprices one item of the user's own claiming expense from the version they believe it is at.
- **2 barrier-race loops per group.** Each round makes an expense with about 70% of its items claimed, then releases together two edits of different items made from one version, with two claims; then, released together, two finalizes (different keys), two voids (different keys), one more edit and four claims or unclaims.
- **3 settle goroutines**, each looping rounds 300 ms apart: a new group of six (two creditors, four debtors), balances read and frozen, then, released together, three settlements per debtor of about 55% of what they owe (two together exceed the debt), then two small follow-ups per debtor. The real workers execute them against the mock provider (median latency 100 ms, log-normal sigma 0.4; 5% declines, 3% each of timeout or 5xx before and after the payment, 10% failed reads, webhooks 20% dropped / 30% duplicated / 30% reordered, up to 200 ms).
- **Every mutating request is a logical operation with one `Idempotency-Key`**, sent once (55%), once and again after the answer (15%), two or three copies at the same instant (15%), or abandoned after 1-5 ms and sent again (15%).

Then it waits for every settlement to be final, lets late webhooks arrive, reads the provider's record of every payment, and runs the checker (`stressrig.Check`). The checker **recomputes from the tables, never from API responses**:
- each ledger transaction, each group and the whole ledger sum to zero;
- each finalized expense's entries equal `allocator.Allocate` + `ledger.ExpenseEntries` over its items, claims, tax and tip, and a void is the exact negation;
- expense status agrees with its postings;
- each succeeded settlement has exactly one posting with exactly its entries;
- one expense or settlement per logical idempotency key, and the rows match what the client was told;
- in the frozen settle groups, the settlements that are not failed fit in what each member owed or was owed;
- every item edit was applied to the version it was made from (read from the edit audit events);
- the ledger agrees with the provider's books (`settlerig.Verify`).

## Configurations

Production, then in the controls build, one at a time: `claim_lock`, `finalize_lock`, `void_lock`, `edit_lock`, `edit_version_check`, `idempotency_check`, `group_lock` off; `resolve_guards` off with `resolve_lock` on; `resolve_lock` off; `resolve_lock` off with the unique index on the settlement posting dropped in the scratch database; all three off (the order the settlement success path meets its layers). The `resolve_*` cases use a mock whose webhooks mostly arrive twice and soon, so resolvers meet at the same settlement. 3 runs each, seeds 1-3. Not disabled: authorization, the append-only and zero-sum ledger rules, CHECK constraints, the conditional update that ends an edit.

## What the program asserts

It writes its files, then exits non-zero otherwise.
- **Production:** every invariant held; no response outside the documented ones; no 5xx; no api error other than a client going away; every settlement final; and the run was **not vacuous**: it counts that retries were answered from the stored response, concurrent duplicates waited and replayed, requests were abandoned, claims were refused for arriving after a finalize, finalizes and voids succeeded, races ran with a finalize landing mid-burst and with two finalizes meeting, two edits from one version met with one refused, a 20 ms sampler of `pg_locks` saw a backend waiting for a lock, and settlements were accepted, refused for capacity, replayed and succeeded.
- **Controls:** each case matches `stressrig.ControlCases`, the same table `make test-controls` uses: the exact set of invariants violated (the three zero-sum invariants must hold in every case, because the database enforces them) and the database layer that must show up in the logged errors.

Per run it reports the violations by invariant (zeros included), what clients saw (5xx, replays, abandoned requests, race counts), the provider's own counters, and the errors the api and the workers logged, counted by cause (a client going away, `unique:<constraint>`, a deadlock, a guarded update that moved no row). The logged errors are the evidence of which layer caught a problem. Every per-run value is listed.

## Limits

- Seeds fix each goroutine's sequence of operations, not the interleaving, so repeats are separate samples.
- A clean production run is evidence under this workload on this machine, not a proof; "0 violations" always comes with the counts of the dangerous things that happened.
- A control that shows no violation and no error is one this workload did not get past. That is not the same as the protection being unnecessary.
- The api, the harness, the workers, the mock and Postgres share one machine, so absolute rates mean little and no throughput claim is made.
- A finalize racing a settlement in the same group is never exercised (settle groups are frozen and separate). The workload's edits only reprice; adding or removing items and confirm are not in the mix.
