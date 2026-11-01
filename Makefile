COMPOSE := docker compose -f deploy/docker-compose.yml
GOOSE   := go tool -modfile=tools/go.mod goose -dir migrations postgres

# Two roles (migration 00004). Migrations run as the owner `fifty`; the api and the
# tests run as the unprivileged `fifty_app`, which cannot UPDATE or DELETE the ledger.
export MIGRATE_DATABASE_URL ?= postgres://fifty:fifty@localhost:5433/fifty?sslmode=disable
export DATABASE_URL         ?= postgres://fifty_app:fifty@localhost:5433/fifty?sslmode=disable

.PHONY: up down reset migrate-up migrate-down migrate-status generate run-api test test-e2e bench-recovery bench-queue bench-settlement

up: ## start Postgres and Dex, wait for Postgres to be healthy
	$(COMPOSE) up -d --build --wait postgres dex

down: ## stop containers, keep data
	$(COMPOSE) down

reset: ## stop containers and delete the database volume
	$(COMPOSE) down -v

migrate-up:
	$(GOOSE) "$(MIGRATE_DATABASE_URL)" up

migrate-down: ## roll back one migration
	$(GOOSE) "$(MIGRATE_DATABASE_URL)" down

migrate-status:
	$(GOOSE) "$(MIGRATE_DATABASE_URL)" status

generate: ## regenerate sqlc code from migrations/ and internal/db/queries/
	go tool -modfile=tools/go.mod sqlc generate

# Dev defaults for the local Dex from deploy/. The api binary requires these to be set.
export OIDC_ISSUER    ?= http://localhost:5556/dex
export OIDC_CLIENT_ID ?= fifty-fifty

run-api:
	go run ./cmd/api

# Integration tests run against the compose Postgres (make up && make migrate-up).
# Without TEST_DATABASE_URL they are skipped, not failed.
# TEST_DATABASE_URL is the app role, so tests exercise the same privileges as production.
# TEST_OWNER_DATABASE_URL is only for tests that must prove a protection holds even
# against the table owner (e.g. the ledger append-only triggers).
export TEST_DATABASE_URL       ?= $(DATABASE_URL)
export TEST_OWNER_DATABASE_URL ?= $(MIGRATE_DATABASE_URL)

test:
	go test ./...

# End-to-end settlement tests: real worker and mock-provider processes, a scratch database
# created and dropped per scenario. Takes minutes; skipped by `make test` unless E2E=1.
test-e2e:
	E2E=1 go test -count=1 -timeout 30m -v ./test/e2e

# Crash-recovery benchmark (bench/README.md). Needs a clean working tree and writes
# bench/results/recovery-*.json and .txt; commit those with the code's commit hash noted.
bench-recovery:
	go run ./bench/recovery

# Queue benchmarks: throughput vs worker count, latency, retry behavior (bench/README.md).
# About 30 minutes. Needs a clean working tree; writes bench/results/queue-*.json and .txt.
bench-queue:
	go run ./bench/queue

# Settlement fault-injection benchmark: real worker and mock-provider processes, a scratch
# database per run (bench/README.md). About 20 minutes. Needs a clean working tree; run it
# as `caffeinate -ims make bench-settlement` on a quiet machine.
bench-settlement:
	go run ./bench/settlement
