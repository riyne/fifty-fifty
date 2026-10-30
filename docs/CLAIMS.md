# Claims ledger

Rule: **nothing goes on the resume or the README unless it has a row here with status `measured` or `built`, plus evidence.**
Numbers come only from real runs. Methodology is recorded next to the number. Estimates are never entered here.

Status values: `planned` (do not use anywhere), `built` (exists, verified by tests), `measured` (number from a recorded run).

## Claims

| # | Claim (resume-style wording, no numbers until measured) | Evidence (test / bench path) | Methodology | Result | Commit | Status |
|---|---|---|---|---|---|---|
| 1 | Ledger stays consistent (zero-sum, no duplicate settlement, shares match claims) under concurrent users | `bench/stress` (TBD) | N users, duration, op mix, hardware, runs: TBD | TBD | TBD | planned |
| 2 | Negative control: same test with protections disabled shows anomalies | `bench/stress` with `unsafe_controls` build tag (TBD) | Same as #1 | TBD | TBD | planned |
| 3 | Idempotent settle-up: retries and duplicate keys never double-record | integration + stress (TBD) | TBD | TBD | TBD | planned |
| 4 | Postgres-backed job queue: throughput and p50/p99 latency vs mock LLM | `bench/queue` (TBD) | Workers, simulated LLM latency, job count: TBD | TBD | TBD | planned |
| 5 | Recovery after `kill -9` of a worker mid-job: no lost jobs, no duplicate effects | `bench/recovery` (TBD) | Trials, lease duration: TBD | TBD | TBD | planned |
| 6 | OIDC login (Auth Code + PKCE) against self-hosted Dex | integration tests (TBD) | n/a | n/a | TBD | planned |
| 7 | Group-scoped authorization verified by cross-group (BOLA) test suite | `test/bola/bola_test.go` | 4 actors (anon, outsider, non-owner member, owner) x 21 scenarios over the 10 group-scoped operations that exist as of M2.5 (4 from M1.3, 3 expense operations, claim, unclaim, finalize); denied requests asserted to leave state unchanged (join codes, memberships, audit, expenses, items, claims); 404 for foreign vs nonexistent ids asserted byte-identical, including a real expense id from another group; a user who is a member of two groups is asserted unable to read, list or claim across them; finalize is refused with 403 for a member who is not the payer (including the group owner) and 404 for non-members, checked to leave expenses, claims, ledger and audit rows unchanged; a guard test fails if any authenticated operation in `api/openapi.yaml` lacks a scenario or a reasoned exemption. Covers only endpoints that exist today; grows with each milestone | passes (`go test ./test/bola`); suite size is NOT a security claim, say "tested" not "secure" | f20d99e (9 operations as of M2.4; first 4 were e611bb6, 7 at 26852d7); the finalize extension is uncommitted, fill in its hash when committed | built |
| 8 | GitHub Actions CI: lint, tests, govulncheck, secret scan | `.github/workflows` (TBD) | n/a | n/a | TBD | planned |
| 9 | Regex PII redaction before LLM call, measured recall on planted synthetic PII | `bench/redaction` (TBD) | Synthetic receipts, PII classes: TBD | TBD | TBD | planned |
| 10 | LLM extraction accuracy on synthetic receipts (real LLM) | `bench/accuracy` (TBD) | ~50 synthetic receipts, metrics: TBD | TBD | TBD | planned |
| 11 | Postgres RLS defense in depth, overhead measured | `test/rls`, `bench/rls` (TBD) | TBD | TBD | TBD | planned |
| 12 | React Native (Expo) client tested on iOS Simulator and Android emulator | mobile/ (TBD) | n/a | n/a | TBD | planned |

## Claims that must NOT be made

- Real money movement, production use, "payments platform", "banking-grade". Settlement is **simulated**.
- Any load figure not produced by a recorded run. Benchmarks against the mock LLM must say "mock LLM with simulated latency of X ms".
- "Minimizes" transfers. Debt simplification is a greedy heuristic and **reduces** them.
- PII "removal". Only regex redaction of specific patterns (card numbers, phones, emails) is implemented; names are not redacted.
- Encryption at rest of receipts (envelope encryption was cut from scope).
- Kubernetes, Terraform, autoscaling (README extension path only, not built).
- Vision-model extraction, Flutter, Swift/iOS-native, multi-currency, push notifications.
- "Tested on iOS/Android" for any platform not actually run. Real-camera capture is untested (simulators only).
- Accuracy numbers from synthetic receipts presented as real-world accuracy.
