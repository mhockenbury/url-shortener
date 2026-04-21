.PHONY: up down migrate migrate-postgres migrate-clickhouse run-api run-worker test tidy build fmt

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
