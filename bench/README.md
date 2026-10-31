# Benchmarks

Every number quoted anywhere comes from a run recorded here, with its environment, and has a row in `docs/CLAIMS.md`. Raw output is committed under `bench/results/`. Benchmarks that use the mock LLM must say "mock LLM with simulated latency of X ms".

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
