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
