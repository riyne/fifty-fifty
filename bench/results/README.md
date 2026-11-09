# Recordings

Raw output of every benchmark recording: a `.txt` summary and a `.json` with every run, the parameters and the environment. Nothing here is edited or deleted. The claim numbers are those of [docs/EVIDENCE.md](../../docs/EVIDENCE.md); the commit is the code that ran.

## Used

| Recording | Commit | Claim |
|---|---|---|
| `stress-production-20261107T225340Z` | c5ee83b | 1: ledger under load, production build |
| `stress-controls-20261106T221435Z` | 2766d9f | 2: negative controls |
| `queue-20261031T085001Z` | 0568c75 | 3: throughput and latency (its `retry` section is not used, see below) |
| `recovery-20261030T234900Z` | 621138a | 4: `kill -9` recovery |
| `queue-20261031T094708Z` | 0568c75 | 5: retries (`retry` scenario only) |
| `settlement-20261101T042323Z` | eb91381 | 6: settlement fault injection |
| `redaction-20261107T044640Z` | 03d2575 | 7: old email pattern, references 1-1000 |
| `redaction-20261108T202918Z` | d8fb754 | 7: new email pattern, references 1-1000 |
| `redaction-20261108T203322Z` | d8fb754 | 7: new email pattern, references 1001-2000 |
| `redaction-20261108T203739Z` | da9d045 | 7: old email pattern, references 1001-2000 |
| `accuracy-20261108T044115Z` | 69368b2 | 8: prompt v1, references 5001-5050 |
| `accuracy-20261109T030135Z` | 98b25c5 | 8: A, prompt v1, references 5101-5150 |
| `accuracy-20261109T030425Z` | 98b25c5 | 8: B, prompt v2, references 5101-5150 |
| `accuracy-20261109T030657Z` | 98b25c5 | 8: C, prompt v2, references 5001-5050 |
| `injection-20261109T032357Z` | 06f8c48 | 9: prompt-injection check |
| `rls-20261107T231125Z` | 8b9b6d9 | 10: cost of row-level security |

## Kept, not used, and why

- **`stress-production-20261106T220029Z`** (da71b03): every invariant held in all three runs, but seeds 1 and 2 were flagged vacuous. The check that the run provoked a rare event (a finalize landing in the middle of a burst of claims) had a floor of 10, chosen from the older workload, and the run produced 8 and 6. The floor was lowered afterwards to 5, then to 1, after seeing these counts. The program exited non-zero.
- **`stress-production-20261106T032136Z`, `stress-controls-20261106T032527Z`** (a72e779): the first recording. It predates item edits, so it has no edit operation and none of the edit controls. Superseded by the two stress recordings above. In the controls file, 2 of 27 runs (`claim_lock` seeds 2 and 3) are marked DOES NOT MATCH on the same kind of check: with the claim lock off, the counter it uses is near zero by construction, while the control's own result (the ledger differing from the claims, 78 / 82 / 83) was seen in all three. Kept as written.
- **`stress-production-20261106T220956Z`** (2766d9f): clean, three runs. Superseded for claim 1 by the 2026-11-07 recording, which has row-level security and the separate worker roles in the code under test.
- **`stress-controls-20261106T221435Z`, three runs marked DOES NOT MATCH**: `edit_version_check` seeds 1 and 2 and `group_lock` seed 2, solely because the same floor (5) was not met (2, 3 and 4 rounds). They matched every other expectation. The recording is used as it is.
- **The `retry` section of `queue-20261031T085001Z`**: three of its nine runs show stalls of about 300 s, because the laptop went to sleep during the recording. Its timings are void. Its outcome counts agree with `queue-20261031T094708Z`, which re-ran `retry` alone under `caffeinate`. The mock's seed is the run index, so the two recordings draw the same faults; re-recording does not add independent samples.

## Recorded before later changes

Not repeated, and stated in the claims they back:
- `stress-controls-*`, `settlement-*`, `queue-*` and `recovery-*` predate row-level security and the separate worker database roles. The `queue` and `recovery` recordings ran the workers as the application role; the programs now use `fifty_worker`.
- `queue-20261031T094708Z` predates the worker honoring a 429 `retry-after`.
- `rls-20261107T231125Z` predates the lock timeout being set in the identity statement, and the balances query joining open settlements.
