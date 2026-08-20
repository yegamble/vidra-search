# vidra-search developer commands. Run `make help` for the list.

.DEFAULT_GOAL := help
SHELL := /bin/bash

# Standalone dev defaults (docker-compose maps postgres to 5433, redis to 6380).
DATABASE_URL ?= postgres://vidra_search:vidra_search@localhost:5433/vidra_search?sslmode=disable
REDIS_URL    ?= redis://localhost:6380/0

# Applying migrations needs no CLI: they are embedded in the binary and applied
# by the binary's own `migrate up` subcommand (see migrations/embed.go +
# internal/dbmigrate), which pins the ledger to `public.vidra_search_migrations`
# — table AND schema — so it never collides with vidra-core's schema_migrations
# when the two services share a database.
#
# MIGRATE_URL is only for the CLI-only `migrate-down` escape hatch below, where
# the ledger name has to travel as the x-migrations-table URL parameter. Keep the
# table name identical to dbmigrate.Table.
MIGRATE_URL := $(DATABASE_URL)&x-migrations-table=vidra_search_migrations

# Build metadata injected into internal/version via -ldflags.
VERSION    ?= 0.1.0
COMMIT     := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_DATE := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
VERSION_PKG := github.com/vidra/vidra-search/internal/version
LDFLAGS := -X $(VERSION_PKG).Version=$(VERSION) -X $(VERSION_PKG).Commit=$(COMMIT) -X $(VERSION_PKG).Date=$(BUILD_DATE)

# Pinned sqlc release. sqlc-verify enforces this exact version so "current"
# means the same thing on every machine and in CI.
SQLC_VERSION := v1.31.1

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

.PHONY: tidy
tidy: ## Sync go.mod/go.sum
	go mod tidy

.PHONY: fmt
fmt: ## Format Go code
	go fmt ./...

.PHONY: fmt-check
fmt-check: ## Fail if any Go file is not gofmt-clean (CI-safe, non-mutating)
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then \
		echo "Not gofmt-clean:"; echo "$$out"; exit 1; fi

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: test
test: ## Run unit tests
	go test ./...

.PHONY: test-race
test-race: ## Run tests with the race detector
	go test -race ./...

.PHONY: cover
cover: ## Run tests with coverage summary
	go test -cover ./...

.PHONY: test-integration
test-integration: ## Run integration tests (-tags=integration); needs DATABASE_URL + REDIS_URL — each test self-skips if unset
	go test -tags=integration -race ./...

.PHONY: build
build: ## Build the api binary into ./bin (injects version metadata)
	go build -ldflags "$(LDFLAGS)" -o bin/api ./cmd/api

.PHONY: run
run: ## Run the api server locally (needs Postgres + Redis)
	go run ./cmd/api

.PHONY: sqlc
sqlc: ## Generate typed query code (requires sqlc installed)
	sqlc generate

.PHONY: sqlc-verify
sqlc-verify: ## Fail if internal/store/sqlcgen is stale vs queries/migrations (non-mutating sqlc diff)
	@if command -v sqlc >/dev/null 2>&1 && [ "$$(sqlc version)" = "$(SQLC_VERSION)" ]; then \
		sqlc diff || { echo "sqlc-verify: generated code is STALE — run 'make sqlc' and commit internal/store/sqlcgen."; exit 1; }; \
	else \
		echo "sqlc-verify: pinned sqlc $(SQLC_VERSION) not on PATH; falling back to 'go run' (slower)"; \
		go run github.com/sqlc-dev/sqlc/cmd/sqlc@$(SQLC_VERSION) diff || { echo "sqlc-verify: generated code is STALE — run 'make sqlc' and commit internal/store/sqlcgen."; exit 1; }; \
	fi

.PHONY: openapi-lint
openapi-lint: ## Lint the OpenAPI contract (requires npx; uses Redocly CLI)
	npx --yes @redocly/cli@1 lint api/openapi.yaml   # pinned 1.x; keep in lock-step with openapi.yml

.PHONY: openapi-verify
openapi-verify: ## Verify routes match api/openapi.yaml (documentation drift guard)
	go test ./internal/api/ -run TestOpenAPIContract

.PHONY: migrate-up
migrate-up: ## Apply the embedded migrations against DATABASE_URL (no CLI needed)
	DATABASE_URL="$(DATABASE_URL)" go run ./cmd/api migrate up

.PHONY: migrate-version
migrate-version: ## Print the migration ledger version + dirty flag (non-zero exit if dirty)
	DATABASE_URL="$(DATABASE_URL)" go run ./cmd/api migrate version

.PHONY: migrate-down
migrate-down: ## Roll back one migration (dev escape hatch; requires the migrate CLI — the binary has no down)
	migrate -path migrations -database "$(MIGRATE_URL)" down 1

# Enforces the one-release schema-compat policy: release N-1's code must keep
# running against release N's schema, which is what makes a rollback a tag flip.
.PHONY: migrate-lint
migrate-lint: ## Fail if any up migration carries destructive DDL (one-release schema-compat policy)
	@bash scripts/migrate-lint.sh

.PHONY: up
up: ## Start the local standalone Docker stack (postgres, redis, migrate, api)
	docker compose up --build

.PHONY: down
down: ## Stop the local Docker stack
	docker compose down

# Also the cure for a Postgres volume created by an older major (pre-18): the
# postgres:18 container refuses to start on it ("database files are incompatible
# with server"). Dev data is disposable — drop the volume and re-migrate.
.PHONY: reset
reset: ## Stop the local Docker stack AND delete its data volumes (fresh Postgres + Redis)
	docker compose down -v

.PHONY: shadow-eval
shadow-eval: ## Run one shadow-evaluation pass over shadow ranker models and exit
	SEARCH_RUN_JOB=shadow_eval SEARCH_WORKERS_ENABLED=false go run ./cmd/api

.PHONY: covis-rollup
covis-rollup: ## Run one co-visitation rollup pass and exit
	SEARCH_RUN_JOB=covis_rollup SEARCH_WORKERS_ENABLED=false go run ./cmd/api

.PHONY: activate-model
activate-model: ## Promote a shadow ranker to active (retires the previous); requires VERSION=ranker-...
	@test -n "$(VERSION)" || { echo "usage: make activate-model VERSION=ranker-YYYYmmddHHMMSS"; exit 1; }
	psql "$(DATABASE_URL)" -v ON_ERROR_STOP=1 -c "BEGIN; \
		UPDATE search.models SET status='retired' WHERE kind='ranker' AND status='active'; \
		UPDATE search.models SET status='active', activated_at=now() WHERE kind='ranker' AND version='$(VERSION)'; \
		COMMIT;"
	@echo "activated ranker $(VERSION); model_loader will hot-swap within its interval."

.PHONY: seed-loadtest
seed-loadtest: ## Seed N synthetic documents for the load test (N via COUNT, default 100000)
	go run ./scripts/loadtest -mode=seed -n=$${COUNT:-100000}

.PHONY: loadtest
loadtest: ## Drive the suggestions endpoint and report p50/p95/p99 (see scripts/loadtest)
	go run ./scripts/loadtest -mode=drive -rps=$${RPS:-50} -duration=$${DURATION:-30s}

.PHONY: check
check: fmt vet test ## Run the standard local gate (fmt, vet, test)

# ci is the single source of truth for the gate. search-ci.yml runs THIS exact
# target, so "passes locally" == "passes in GitHub". Keep them in lock-step.
.PHONY: ci
ci: fmt-check vet migrate-lint openapi-verify sqlc-verify test-race ## Canonical CI gate (run locally to mirror GitHub exactly)
	@echo "ci: gate passed (fmt-check, vet, migrate-lint, openapi-verify, sqlc-verify, test-race)."
