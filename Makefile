# Ensure the Go toolchain and go-installed tools (e.g. swag) are on PATH
# for targets invoking them. Matches the install locations from notes.md.
export PATH := $(PATH):/usr/local/go/bin:$(HOME)/go/bin

.PHONY: up down migrate migrate-postgres migrate-clickhouse run-api run-worker test tidy build fmt openapi openapi-check openapi-tools

# Compose services (postgres, redis, clickhouse) must be healthy before migrate.
up:
	docker compose up -d postgres redis clickhouse

down:
	docker compose down

# Apply all pending migrations. Idempotent — individual migrations use
# IF NOT EXISTS / ON CONFLICT DO NOTHING so re-running is a no-op.
migrate: migrate-postgres migrate-clickhouse

migrate-postgres:
	@echo "==> applying Postgres migrations"
	@for f in migrations/*.sql; do \
		echo "--> $$f"; \
		docker compose exec -T postgres psql -v ON_ERROR_STOP=1 -U shortener -d shortener < "$$f" || exit 1; \
	done

migrate-clickhouse:
	@echo "==> applying ClickHouse migrations"
	@for f in migrations/clickhouse/*.sql; do \
		echo "--> $$f"; \
		docker compose exec -T clickhouse clickhouse-client \
			--user shortener --password shortener --database shortener \
			--multiquery < "$$f" || exit 1; \
	done

# Dev loop. For graceful-shutdown testing use `make build && ./bin/api` —
# `go run` does not forward SIGTERM cleanly to the child process.
run-api:
	go run ./cmd/api

run-worker:
	go run ./cmd/worker

test:
	go test ./...

tidy:
	go mod tidy

build:
	go build -o bin/api ./cmd/api
	go build -o bin/worker ./cmd/worker

fmt:
	go fmt ./...

# --- OpenAPI / Swagger ---
# Spec is Swagger 2.0 (what swag emits natively). It is regenerated from
# handler godoc annotations; do not hand-edit docs/openapi/. See
# docs/tradeoffs.md for the generator choice rationale.

# Regenerate the spec. Run this after changing any handler or request/response type.
openapi:
	swag init -g cmd/api/main.go --dir . --output docs/openapi --parseInternal --parseDependency=false

# Drift check: regenerate and fail if the result differs from HEAD. Meant for
# CI and pre-push hooks so nobody merges code whose committed spec is stale.
# Comparing against HEAD (not the index) is what makes this honest — we want
# "does regeneration match what's committed," not "did anyone stage this."
openapi-check: openapi
	@git diff --exit-code HEAD -- docs/openapi/ || { \
		echo "ERROR: docs/openapi/ is out of date vs committed spec."; \
		echo "       Run 'make openapi' and commit the result."; \
		exit 1; \
	}

# One-time install of the swag CLI. Installs into \$GOPATH/bin (usually ~/go/bin).
openapi-tools:
	go install github.com/swaggo/swag/cmd/swag@latest
