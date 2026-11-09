# What row-level security costs

The same seeded database and the same closed loop of concurrent workers, run three ways, to separate the cost of the identity seam from the cost of the policies. Database layer only. Results: [claim 10](../../docs/EVIDENCE.md).

    caffeinate -ims make bench-rls     # about 8 minutes; --reps, --measure, --warmup shrink it (needs --allow-dirty, never committed)

## What is measured

8 workers, each picking a random user and an operation, no think time, in three variants:
- **`baseline`**: row-level security off, queries on the plain pool (what the api did before the policies existed: autocommit reads, a transaction for writes).
- **`identity_only`**: row-level security off, queries through `internal/dbscope`. A read is `BEGIN`, `set_config`, the statement, `COMMIT`; a write is a transaction that starts with `set_config`.
- **`policies_on`**: row-level security enabled and forced on the 14 tables, through `dbscope`.

`identity_only` against `baseline` is the cost of the seam; `policies_on` against `identity_only` is the cost of evaluating the policies. The variants are switched with `ALTER TABLE ... ENABLE/DISABLE ROW LEVEL SECURITY` run by the owner while the workers are idle; nothing in the product can do that.

**The mix** (percent): list groups 10, list expenses 25, get one expense with its items 30, group balances 20, claim then unclaim 15 (two transactions that net to nothing). The queries are the sqlc queries the handlers call. Users belong to one group, or two for every fifth user.

**Data** (per repetition, a fresh scratch database, bulk-loaded by the owner): 200 groups of 6 members, 1,200 users, 40,000 expenses (half finalized with a posting of two ledger entries, half claiming), 200,000 items, 100,000 claims, `ANALYZE` after seeding.

**Procedure.** 6 repetitions, each on its own database, the variants in a rotating order (all 6 permutations) so that order or a warming cache cannot favor one; 3 s warm-up and 15 s timed per variant; the pool is opened at full size first. Throughput is completed operations per second; latency is nearest-rank p50 and p99 per operation, then the median over repetitions. Ratios are computed per repetition, then summarized.

## Controls the program enforces

It stops and records the failure otherwise.
- Each variant reads the catalog: policies forced on 14 tables in `policies_on`, on none in the other two. A switch that did not take effect cannot be reported as a measurement.
- Each variant counts the expenses an unguarded `SELECT count(*)` sees as a user: all of them with the policies off, only the user's groups with them on.
- A fixed list of 300 questions is asked in every variant and the row counts compared: row-level security must not change what a legitimate query returns.
- A failed operation fails the run. All of these were shown to trigger by breaking the program on purpose.
- Repetition 1 also records `EXPLAIN ANALYZE` for three queries with the policies on, to show the group list is a hashed subplan evaluated once per statement, not once per row.

## Not claimed

Anything about a real network (here a round trip costs a few tenths of a millisecond; with a network between the api and the database each extra round trip costs more, and the seam adds three to each autocommit read); other data sizes, mixes or hosts; the worker and webhook paths; latency of the api over HTTP; row-level security in general. The mix and the data are the author's choice.
