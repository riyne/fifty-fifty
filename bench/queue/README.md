# Queue throughput, latency and retries

Real `benchworker` processes (4 jobs in flight each, 1 s poll, 30 s lease: the worker defaults) take `mock.extract` jobs from Postgres. Each job makes one mock LLM call, then writes one effect row inside the completion transaction. The outcome is read from Postgres. The mock LLM runs in the harness process. Each configuration is run 3 times with a different mock seed, and every per-run value is reported. Results: [claims 3 and 5](../../docs/EVIDENCE.md).

    caffeinate -ims make bench-queue     # about 30 minutes; --help for the knobs; --scenarios retry runs one

## Scenarios

- **`throughput-llm`**: closed loop. 40 jobs per slot are enqueued up front (160 / 320 / 640 / 1,280 for 1 / 2 / 4 / 8 workers) and drained, against a mock LLM with simulated latency of 800 ms median (log-normal, sigma 0.4). Throughput is bounded by `slots / mean latency`, so it is reported next to that bound; the ratio says what the queue costs on top of the LLM wait.
- **`throughput-queue`**: closed loop, 10,000 jobs, mock LLM with simulated latency of 0 ms. The handler still makes a local HTTP call and writes a row, so it is a ceiling for this harness, not the queue alone.
- **`latency`**: open loop. Jobs arrive as a Poisson process at 50% of the theoretical capacity for 30 s after a 2 s warm-up. Reports end-to-end time (enqueue to success) and queue wait (enqueue to the handler's start), p50 and p99 per run.
- **`latency-low`**: the same at 1 job/s with 1 and 4 workers, so workers are mostly idle. This shows what polling costs when nothing is busy.
- **`retry`**: 500 jobs on 4 workers while the mock injects 429s, 5xx and malformed 200s: none; 10% / 5% / 5%; 20% / 15% / 10%. `max_attempts` 5. Reports success and dead-letter rates, jobs by number of attempts, the fault rates the mock actually produced, and the dead-letter rate expected if every attempt failed independently.

## Definitions and limits

- Throughput is `(succeeded - 1) / (time from the first success to the last)`. It leaves out the ramp-up and includes the tail where slots go idle.
- All times are the database clock.
- The throughput scenarios run the handler lean (no bookkeeping writes beyond the effect row). The latency scenarios need bookkeeping rows for queue wait, so they run instrumented.
- The `retry` scenario ignores the 429 `retry-after` header; the recordings were made before the worker honored it.
- The recordings ran the workers as the application database role. `internal/harness/benchwork` now runs them as `fifty_worker`, the role of a production worker.
- One machine, Postgres in Docker Desktop with the default `max_connections`, a mock LLM. The development database also holds old rows from other tests (the count is recorded). The harness deletes each run's rows afterwards.
