# Benchmarks

Seven programs that produce the measured numbers in [docs/EVIDENCE.md](../docs/EVIDENCE.md). Each has its own README with its method and its limits. Raw output of every recording is in [results/](results/), indexed in [results/README.md](results/README.md).

| Benchmark | Run it | Roughly | What it shows | Evidence |
|---|---|---|---|---|
| [stress](stress/) | `make bench-stress` | 51 min | the ledger under concurrent load; the same run with one protection off | claims 1, 2 |
| [queue](queue/) | `make bench-queue` | 30 min | queue throughput, latency, retries | claims 3, 5 |
| [recovery](recovery/) | `make bench-recovery` | | `kill -9` of a worker mid-job | claim 4 |
| [settlement](settlement/) | `make bench-settlement` | 20 min | settlement under injected provider faults | claim 6 |
| [redaction](redaction/) | `make bench-redaction` | 4 min | how much planted personal data the redaction removes | claim 7 |
| [accuracy](accuracy/) | `make bench-accuracy` | 3 min | a real model reading receipts; the prompt-injection check. **Costs money.** | claims 8, 9 |
| [rls](rls/) | `make bench-rls` | 8 min | what row-level security costs | claim 10 |

## Rules for a recording

- **Clean git tree.** The result records the commit, so it has to be the code that ran. A smoke run (`--allow-dirty`, shrunk with flags) is never committed.
- **Quiet machine, awake.** Close heavy programs and run `caffeinate -ims make bench-...`. A span far above the expected duration means the machine slept and the run is void; the harnesses do not detect sleep.
- **Prerequisites.** `make up && make migrate-up` (the benchmarks connect as the database owner to create scratch databases) for everything except `redaction` (Tesseract only) and `accuracy` (Tesseract and an API key).
- **Mocks are named.** A result that uses the mock LLM says "mock LLM with simulated latency of X ms"; one that uses the mock payment provider says "mock payment provider with simulated latency of X ms". Settlement is simulated.
- **Seeds are run indices.** Repeats are separate samples of something that cannot be repeated exactly, not independent draws of the same experiment.
- **Recordings are never edited or deleted.** If one is flawed, [results/README.md](results/README.md) says why it is not used.
- Each result file records the commit, machine, Docker limits, Postgres version, parameters and every run, so a number can be checked without trusting this page.
