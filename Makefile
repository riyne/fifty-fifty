# bash, not whatever /bin/sh is: the CI targets use pipefail.
SHELL := /bin/bash

COMPOSE := docker compose -f deploy/docker-compose.yml
GOOSE   := go tool -modfile=tools/go.mod goose -dir migrations postgres

# Two roles (migration 00004). Migrations run as the owner `fifty`; the api and the
# tests run as the unprivileged `fifty_app`, which cannot UPDATE or DELETE the ledger.
export MIGRATE_DATABASE_URL ?= postgres://fifty:fifty@localhost:5433/fifty?sslmode=disable
export DATABASE_URL         ?= postgres://fifty_app:fifty@localhost:5433/fifty?sslmode=disable

.PHONY: up down reset migrate-up migrate-down migrate-status generate run-api test test-unsafe test-controls test-e2e bench-recovery bench-queue bench-settlement bench-stress ci ci-format ci-vet ci-generate ci-test ci-vuln

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

# The negative-control seams (internal/controls) exist only with the unsafe_controls
# tag. This runs their tests, and the whole suite once with the tag and every
# protection still ON, which must behave exactly like the production build.
test-unsafe:
	go test -count=1 -tags unsafe_controls ./...

# The negative controls at smoke size: the stress workload with one protection switched
# off at a time, expecting the harness to see the effect (or the layer that caught it).
# Starts real processes; about 5 minutes. Build tag unsafe_controls; never ship it.
test-controls:
	E2E=1 go test -tags unsafe_controls -count=1 -run StressControls -v -timeout 30m ./test/e2e

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

# Ledger stress benchmark and its negative controls (bench/README.md): the production
# build first (nothing disabled), then the unsafe_controls build with one protection off at
# a time. Needs a clean git tree and a quiet machine; run as `caffeinate -ims make
# bench-stress`. Writes bench/results/stress-production-*.{json,txt} and
# stress-controls-*.{json,txt}. The unsafe_controls build must never be shipped.
bench-stress:
	go run ./bench/stress
	go run -tags unsafe_controls ./bench/stress

# CI. .github/workflows/ci.yml starts Postgres, migrates, then runs `make ci`, so every
# gate can be run on this machine first (needs `make up && make migrate-up`).
# Not in CI: the end-to-end and stress tests, make test-controls and the benchmarks (real
# processes, minutes, timing); lint and secret scanning (not built).
ci: ci-format ci-vet ci-generate ci-test ci-vuln

ci-format:
	@bad="$$(gofmt -l .)"; if [ -n "$$bad" ]; then echo "not gofmt-formatted:"; echo "$$bad"; exit 1; fi

# Both builds: the production one and the one with the negative-control seams.
ci-vet:
	go vet ./...
	go vet -tags unsafe_controls ./...

# Generated sqlc code must be what the migrations and queries produce.
ci-generate:
	go tool -modfile=tools/go.mod sqlc generate
	@git diff --exit-code -- internal/db || (echo "make generate changes internal/db: commit the regenerated code"; exit 1)
	@new="$$(git ls-files --others --exclude-standard -- internal/db)"; if [ -n "$$new" ]; then echo "make generate creates untracked files:"; echo "$$new"; exit 1; fi

# One package at a time: the global reaper and cleanup jobs make tests in different
# packages do each other's work when they run in parallel, and the lease tests are timing
# sensitive. REQUIRE_DATABASE=1 makes a missing database a failure, not a skip. The log
# (git-ignored) is kept for reading, and the skipped tests are listed so that a skip that
# should not be there is visible. Then the whole suite once more with the unsafe_controls
# tag and nothing disabled.
ci-test: export REQUIRE_DATABASE = 1
ci-test:
	@set -o pipefail; go test -count=1 -p 1 -v ./... 2>&1 | tee ci-test.log | grep -E "^(--- FAIL|FAIL|ok|panic)"
	@echo "skipped tests:"; grep -E "^\s*--- SKIP" ci-test.log | sed 's/ (.*//' | sort | uniq -c
	go test -count=1 -p 1 -tags unsafe_controls ./...

# Known vulnerabilities in the dependencies and in the standard library that the code reaches.
ci-vuln:
	go tool -modfile=tools/go.mod govulncheck ./...
