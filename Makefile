COMPOSE := docker compose -f deploy/docker-compose.yml
GOOSE   := go tool -modfile=tools/go.mod goose -dir migrations postgres

export DATABASE_URL ?= postgres://fifty:fifty@localhost:5433/fifty?sslmode=disable

.PHONY: up down reset migrate-up migrate-down migrate-status generate run-api test

up: ## start Postgres and Dex, wait for Postgres to be healthy
	$(COMPOSE) up -d --build --wait postgres dex

down: ## stop containers, keep data
	$(COMPOSE) down

reset: ## stop containers and delete the database volume
	$(COMPOSE) down -v

migrate-up:
	$(GOOSE) "$(DATABASE_URL)" up

migrate-down: ## roll back one migration
	$(GOOSE) "$(DATABASE_URL)" down

migrate-status:
	$(GOOSE) "$(DATABASE_URL)" status

generate: ## regenerate sqlc code from migrations/ and internal/db/queries/
	go tool -modfile=tools/go.mod sqlc generate

# Dev defaults for the local Dex from deploy/. The api binary requires these to be set.
export OIDC_ISSUER    ?= http://localhost:5556/dex
export OIDC_CLIENT_ID ?= fifty-fifty

run-api:
	go run ./cmd/api

# Integration tests run against the compose Postgres (make up && make migrate-up).
# Without TEST_DATABASE_URL they are skipped, not failed.
export TEST_DATABASE_URL ?= $(DATABASE_URL)

test:
	go test ./...
