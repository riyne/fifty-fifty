# Settlement fault injection

Settlements arrive at a steady rate and are executed by real worker processes against a real mock-payment-provider process that loses responses, never answers, declines, and sends duplicate, delayed, dropped and reordered webhooks. Afterwards the ledger is compared with the provider's own record of every payment. **Settlement is simulated**: the provider is a mock, and every result says "mock payment provider with simulated latency of 300 ms". Results: [claim 6](../../docs/EVIDENCE.md).

    caffeinate -ims make bench-settlement   # about 20 minutes; --mixes 1,3 runs some mixes; --runs and --settlements shrink it (needs --allow-dirty, never committed)

## What one run does

Creates a scratch database (so the jobs table starts empty whatever the development database holds). Starts a real `mockpay` process with the mix's faults, the api's webhook handler in the harness process, and 3 real `worker` processes (4 jobs in flight each, 1 s poll, 5 s lease). Then 300 settlements arrive as a Poisson process at 8/s, each inserted the way the create-settlement handler inserts it: the settlement, its execute job and an audit event in one transaction. **The api's authentication, group lock and capacity check are not involved** (they need user tokens a benchmark cannot mint; `internal/settlements` tests them). It waits until every settlement is final, lets duplicate webhooks finish arriving, then runs `settlerig.Verify` against the provider's record.

## Mixes

3 runs each, seeds 1-3. The mock's latency is log-normal, median 300 ms, sigma 0.4; a timed-out connection is held 3 s and aborted.
1. no faults;
2. lost responses (10% timeout after the payment, 10% 5xx after it) with webhooks delivered: 30% duplicated, delayed up to 200 ms, 30% of payments with the terminal event overtaking the first;
3. the same lost responses, no webhooks, so only reconcile can learn the result;
4. requests that never arrive (10% timeout before, 10% 5xx before), webhooks as in 2, so reconcile must find nothing and send the request again;
5. everything: 5% declines, 5% each of the four create faults, 10% failed reads, webhooks dropped 20% / duplicated 30% / reordered 30% / delayed up to 300 ms, and one worker killed with SIGKILL after half the settlements have arrived.

## What the program asserts

It exits non-zero otherwise. No anomaly from `Verify`: every succeeded settlement has exactly one ledger posting and exactly one succeeded payment at the provider; nothing is charged but recorded failed; no payment is created twice; amounts and parties match; every group sums to zero. All settlements final, no dead jobs, and succeeded = ledger postings = provider charges.

Per run it reports final statuses, end-to-end time (creation to final) p50/p90/p99/max, for settlements recorded `unknown` the time from unknown to final, and the provider's own counters (create calls, replays, charges, declines, each fault, webhook events dropped / duplicated / reordered), so the faults actually injected are on record, not only the configured rates.

## What shapes the numbers

- **Unknown-to-final time is mostly the reconcile schedule**, a set of placeholders (first check 5 s after a settlement becomes unknown, doubling to 5 minutes, 12 checks). It measures those constants, not payment providers or the design.
- End to end includes up to one poll interval to be claimed, the provider's simulated latency, and any queueing. At 8/s against 12 slots the system is far below capacity.
- The mock draws faults from (seed, settlement id, call number) and settlement ids are random, so which settlements fault differs between runs even with the same seed.
- One machine, a mock provider, 3 workers, 300 settlements per run. This is a fault-injection benchmark with a correctness assertion, not a throughput benchmark.
- The recording ran the workers as the application database role and predates row-level security.
