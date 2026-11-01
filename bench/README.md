# Benchmarks

Every number quoted anywhere comes from a run recorded here, with its environment, and has a row in `docs/CLAIMS.md`. Raw output is committed under `bench/results/`. Benchmarks that use the mock LLM must say "mock LLM with simulated latency of X ms", and those that use the mock payment provider must say "mock payment provider with simulated latency of X ms" (settlement is simulated).

## Crash recovery (`bench/recovery`, CLAIMS row 5)

`make bench-recovery` (or `go run ./bench/recovery --help`). Needs `make up && make migrate-up`, and a clean git working tree, so the commit hash recorded in the result is the code that ran.

**What one trial does.** Enqueue 24 `mock.extract` jobs (an LLM call, then one effect row written inside the completion transaction). Start 3 real `benchworker` processes (4 jobs in flight each). When the victim worker has 4 jobs in flight, wait a random time in [0, 1 s), then send it SIGKILL (checked: the process must die from that signal). The 2 survivors finish everything. Then read Postgres: lost jobs, duplicate and missing effects, and, for each job the victim held, the time from the kill to that job's success.

**Parameters.** Lease 3 s and 10 s, 20 trials each; poll interval 1 s (the worker default); mock LLM with simulated latency of 800 ms median, log-normal sigma 0.4 (a fresh seed per trial); `max_attempts` 5. The mock runs in the harness process, so it is never killed.

**What it measures, and what shapes it.**
- Recovery time is kill-to-success on the database clock. It contains the remaining lease of the orphan (a job's lease is extended by heartbeats every lease/3, so a long job has 2/3 to all of its lease left when the worker dies, and a short job has what is left of its single claim), then up to one poll interval for a survivor to notice, then the orphan's own run (mock latency), plus any older queued jobs that are claimed first. With 24 jobs and 8 surviving slots, that backlog drains in a few seconds, before a 10 s lease expires; with a 3 s lease some orphans may wait behind it. The results report both per orphan and per trial (until the last orphan finished).
- Time to recovery is therefore set by the lease, and a shorter lease recovers faster at the cost of more heartbeats and a higher chance that a slow-but-alive worker loses a job (the fencing token makes that safe, not free).
- The kill point is "during the LLM call" (the victim's jobs are waiting on the mock). The "mid-completion-transaction" kill is a correctness test (`internal/benchwork/recovery_test.go`), not part of the timed benchmark.

**What it does not show.** One machine, Postgres in a Docker Desktop VM, a mock LLM, a single worker killed per trial. It says nothing about network partitions, a killed Postgres, clock skew between machines (all times are the database clock), or real LLM behavior. Trials with no orphan (the victim happened to be between jobs) are counted and excluded from timings.

**Reading the result.** `bench/results/recovery-<UTC timestamp>.txt` is the summary; the `.json` has every trial, the parameters and the environment (machine, Docker limits, Postgres version, commit, load average).

## Queue throughput, latency and retries (`bench/queue`, CLAIMS row 4)

`make bench-queue` (about 30 minutes; `go run ./bench/queue --help` for the knobs; `--scenarios retry` runs one scenario). Same preconditions as above, and stop the machine from sleeping while it runs (`caffeinate -ims make bench-queue` on macOS): a sleep during a run shows up as a span of minutes instead of seconds, and the harness does not detect it. Every scenario starts real `benchworker` processes (4 jobs in flight each, 1 s poll, 30 s lease, the worker defaults), enqueues `mock.extract` jobs, and reads the outcome from Postgres. The mock LLM runs in the harness process. Each configuration is run 3 times with a different mock seed; the per-run values are all reported, not just a summary.

**Scenarios.**
- `throughput-llm`: closed loop. 40 jobs per slot are enqueued up front (160 / 320 / 640 / 1280 for 1 / 2 / 4 / 8 workers), then drained. Mock LLM with simulated latency of 800 ms median (log-normal, sigma 0.4). With that latency throughput is bounded by `slots / mean latency`, so the result is reported next to that bound; the ratio says how much the queue costs on top of the LLM wait, and the absolute number mostly says how many slots there were.
- `throughput-queue`: closed loop, 10,000 jobs, mock LLM with simulated latency of 0 ms. This takes the LLM wait out and shows what the queue sustains. The handler still makes one local HTTP call and writes one row, so it is not the queue alone.
- `latency`: open loop. Jobs arrive as a Poisson process at 50% of the theoretical capacity for 30 s after a 2 s warm-up. Reported: end-to-end time (enqueue to success) and queue wait (enqueue to the handler's start), p50 and p99 per run.
- `latency-low`: the same at 1 job/s with 1 and 4 workers, so workers are mostly idle. This is the number that shows what polling costs when nothing is busy, and the input to the `LISTEN/NOTIFY` decision.
- `retry`: 500 jobs on 4 workers with the mock injecting 429s, 5xx and malformed 200s: none; 10% / 5% / 5%; 20% / 15% / 10%. `max_attempts` 5. Reported: success and dead-letter rates, jobs by number of attempts, attempts per job, the fault rates the mock actually produced, and the dead-letter rate you would expect if every attempt failed independently with the configured probability.

**Definitions and limits.**
- Throughput is `(succeeded - 1) / (time from the first success to the last)`. It leaves out the ramp-up before the first job finishes and includes the tail where slots go idle.
- All times are the database clock (`created_at`, `finished_at`, `clock_timestamp()` in the bookkeeping rows).
- The throughput scenarios run the handler in lean mode (no bookkeeping writes beyond the effect row) so the measured work is the queue's plus one insert. The latency scenarios need the bookkeeping rows for queue wait, so they run instrumented; each run's raw result records how many bookkeeping rows were written.
- The retry scenario ignores the 429 `retry-after` header: the worker backs off on its own jittered schedule (base 2 s, cap 5 minutes). Honoring the header is not built.
- One machine; Postgres in Docker Desktop with the default `max_connections` (the harness stays well under it); a mock LLM. The dev database also holds old rows from other tests (the count is recorded). Numbers from another machine, a real LLM or a bigger table would differ.
- The harness deletes each run's rows afterwards.

**Recorded runs.**
- `bench/results/queue-20261031T085001Z.{json,txt}`: all scenarios, commit 0568c75. The `throughput-llm`, `throughput-queue`, `latency` and `latency-low` sections are used for CLAIMS row 4. Its `retry` section is kept but not used: 3 of its 9 runs have about 300 s stalls, and the machine went to sleep by accident during that recording.
- `bench/results/queue-20261031T094708Z.{json,txt}`: `retry` only, commit 0568c75, run under `caffeinate`. Used for CLAIMS row 18. The mock's seed is the run index, so the same fault draws repeat across recordings (the request counts of the two `retry` recordings are identical); the 3 runs per mix are three seeds, and re-recording does not add independent samples.

## Settlement fault injection (`bench/settlement`, CLAIMS row 19)

`caffeinate -ims make bench-settlement` (about 20 minutes; `go run ./bench/settlement --help` for the knobs; `--mixes 1,3` runs some mixes, `--runs`/`--settlements` shrink it for a smoke run, which needs `--allow-dirty` and must not be committed). Needs `make up && make migrate-up` (it connects as the owner to create databases), a clean git working tree, and a quiet machine: close games and other heavy programs, and keep the machine from sleeping. **Settlement is simulated**: the provider is a mock, and every result says "mock payment provider with simulated latency of X ms".

**What one run does.** Creates a scratch database (`fifty_e2e_<random>`, every migration applied, dropped afterwards) so it starts with an empty jobs table whatever the development database holds. Starts a real `mockpay` process with the mix's faults, the api's webhook handler in the harness process, and 3 real `worker` processes (4 jobs in flight each, 1 s poll, the worker default; 5 s lease). Then 300 settlements arrive as a Poisson process at 8/s (seeded by the run number), each inserted the way the create-settlement handler inserts it: the settlement, its execute job and an audit event in one transaction. The API's authentication, group lock and capacity check are **not** involved and not measured (that needs user tokens a benchmark program cannot mint; they are tested in `internal/settlements`). It waits until every settlement is final, lets duplicate webhooks finish arriving, then compares the ledger with the provider's own record of every payment (`settlerig.Verify`) and reads the timings from Postgres.

**Mixes** (3 runs each, seeds 1-3; the mock's latency is log-normal, median 300 ms, sigma 0.4; a timed-out connection is held 3 s and aborted, shorter than the worker's 10 s client timeout, so timeouts do not saturate the 12 slots):
1. no faults;
2. lost responses (10% timeout after the payment, 10% 5xx after it) with webhooks delivered: 30% duplicated, delayed up to 200 ms, 30% of payments with the terminal event overtaking the first;
3. the same lost responses, no webhooks, so only reconcile can learn the result;
4. requests that never arrive (10% timeout before, 10% 5xx before), webhooks as in 2, so reconcile must find nothing and send the request again;
5. everything: 5% declines, 5% each of the four create faults, 10% failed reads, webhooks dropped 20% / duplicated 30% / reordered 30% / delayed up to 300 ms, and one worker killed with SIGKILL after half the settlements have arrived.

**What is asserted (the run fails, and the program exits non-zero, otherwise).** No anomaly from `Verify`: every succeeded settlement has exactly one ledger posting and exactly one succeeded payment at the provider; nothing is charged but recorded failed; no payment is created twice; amounts and parties match; every group sums to zero. All settlements final. No dead jobs. Succeeded = ledger postings = provider charges.

**What is reported.** Per run: final statuses; the counts above; **end to end** (the settlement row's creation to it becoming final) p50/p90/p99/max; for the settlements that were recorded `unknown`, **unknown to final** min/median/p90/max; the provider's own counters (create calls, replays, payments, charges, declines, each fault, reads, webhook events dropped/duplicated/reordered/delivered/refused), so the faults actually injected are on record and not only the configured rates; execute jobs that needed a second attempt; reconcile jobs created; watchdog requeues. Across the 3 runs: the median and range of the per-run p50 and p99. All times are the database clock.

**What shapes the numbers.**
- **Unknown to final is mostly the reconcile schedule**, which is a set of placeholders (first check 5 s after the settlement becomes unknown, doubling to 5 minutes, 12 checks), not a property of payment providers or of the design. With no webhooks, a settlement that became unknown resolves at the first check, so the figure is about the 5 s constant plus a read; with a re-send it can take several checks. Webhooks, when they arrive, resolve it sooner and earlier than reconcile.
- End to end includes up to one poll interval (1 s) to be claimed, the provider's simulated latency, and any queueing. At 8/s against 12 slots and a 300 ms mean latency the system is far below capacity, so queueing is small; a timed-out call occupies a slot for 3 s.
- The mock draws its faults from (seed, settlement id, call number), and settlement ids are random, so which settlements get faults differs between runs even with the same seed; the arrival times repeat for the same seed (the same seeds are used for every mix).
- One machine running the workers, the mock, the harness and Postgres 17 in Docker Desktop (no container limits); a mock provider; 3 worker processes and 300 settlements per run. Numbers from another machine, a real provider, more workers or sustained load would differ. This is a fault-injection benchmark with a correctness assertion, not a throughput benchmark.
