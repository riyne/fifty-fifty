# Fifty Fifty

Group expense splitting: photograph a receipt, an LLM itemizes it, friends claim their items, and balances are tracked and settled through a **simulated** payment provider.

> **Status: early development (milestone M1 of 9).** Built so far: Go API skeleton, OIDC auth (Dex), groups, join codes, idempotency keys, and an authorization test suite. Not built yet: expenses, ledger, job queue, settlement, receipt pipeline, mobile app. See [docs/DESIGN.md](docs/DESIGN.md) for the plan and [docs/CLAIMS.md](docs/CLAIMS.md) for what is and is not claimed.

- Synthetic data only. No real money moves anywhere; the payment provider is a mock.
- Every number or capability claimed about this project must appear in [docs/CLAIMS.md](docs/CLAIMS.md) with its evidence.

## Run locally

Needs Go 1.27+ and Docker. Everything below is dev-only with throwaway credentials.

    make up              # Postgres (localhost:5433) and Dex (localhost:5556)
    make migrate-up      # apply migrations
    make test            # unit + integration tests (integration tests need the database above)
    make run-api         # API on :8080

    TOKEN=$(scripts/dev-token.sh alice)     # alice, bob or carol; password is "password"
    curl -H "Authorization: Bearer $TOKEN" localhost:8080/v1/me

`make generate` regenerates the sqlc code after editing `migrations/` or `internal/db/queries/`.
