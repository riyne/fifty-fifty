# Crash recovery

Kills a worker process with `kill -9` while it holds jobs and measures how long the orphaned jobs take to finish, and whether any job was lost or ran twice. Results: [claim 4](../../docs/EVIDENCE.md).

    make bench-recovery        # --help for the knobs

## What one trial does

Enqueues 24 `mock.extract` jobs (a mock LLM call, then one effect row written inside the completion transaction). Starts 3 real `benchworker` processes (4 jobs in flight each). When the victim has 4 jobs in flight, waits a random time in [0, 1 s) and sends it SIGKILL (checked: the process must die from that signal). The two survivors finish everything. Then it reads Postgres: lost jobs, duplicate and missing effects, and, for each job the victim held, the time from the kill to that job's success.

**Parameters.** Lease 3 s and 10 s, 20 trials each; poll interval 1 s; mock LLM with simulated latency of 800 ms median, log-normal sigma 0.4 (a fresh seed per trial); `max_attempts` 5. The mock runs in the harness process, so it is never killed.

## What shapes the result

- Recovery time is kill-to-success on the database clock. It contains the orphan's remaining lease (heartbeats extend a lease every lease/3, so a long job has 2/3 to all of its lease left), then up to one poll interval for a survivor to notice, then the orphan's own run, plus any older queued jobs claimed first. Results are reported per orphan and per trial (until the last orphan finished).
- So recovery is set by the lease. A shorter lease recovers faster at the cost of more heartbeats and a higher chance that a slow-but-alive worker loses a job (the fencing token makes that safe, not free).
- The kill point is "during the model call". The "mid-completion-transaction" kill is a correctness test (`internal/harness/benchwork/recovery_test.go`), not part of the timed benchmark.
- Trials where the victim happened to hold no job are counted and excluded from timings.

## Limits

One machine, Postgres in a Docker Desktop VM, a mock LLM, one worker killed per trial. Nothing about network partitions, a killed Postgres, clock skew (all times are the database clock) or a real LLM. The recording ran the workers as the application database role; the program now uses `fifty_worker`.
