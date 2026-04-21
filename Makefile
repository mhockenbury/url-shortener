# Ensure the Go toolchain and go-installed tools (e.g. swag) are on PATH
# for targets invoking them. Matches the install locations from notes.md.
export PATH := $(PATH):/usr/local/go/bin:$(HOME)/go/bin

.PHONY: up down migrate migrate-postgres migrate-clickhouse run-api run-worker test tidy build fmt openapi openapi-check openapi-tools load-redirect-hit load-redirect-cold up-app down-app restart-app status-app logs-api logs-worker up-all down-all _wait-deps

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
# We delete the generated docs.go — it's a Go file that embeds the spec for
# programs serving Swagger UI, which we don't do. Keeping it would pull the
# swaggo/swag runtime into our module for no benefit.
openapi:
	swag init -g cmd/api/main.go --dir . --output docs/openapi --parseInternal --parseDependency=false
	@rm -f docs/openapi/docs.go

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

# --- Load tests (k6) ---
# Scenarios live in scripts/k6/; see docs/benchmarks.md for what each measures.
# Defaults target localhost:8080 against a running API binary; override BASE_URL
# or per-scenario knobs (RPS, DURATION, POOL_SIZE) via env.

load-redirect-hit:
	k6 run scripts/k6/01_redirect_cache_hit.js

load-redirect-cold:
	k6 run scripts/k6/02_redirect_cold_start.js

# --- App process lifecycle (background) ---
# Runs the compiled api + worker binaries in the background with PID files and
# log files under run/. Preserves the graceful-shutdown behavior — signals go
# straight to the binary (unlike `go run`, which swallows SIGTERM). Logs
# truncate on start, so each run starts fresh.

RUN_DIR := run

up-app: build check-deps
	@mkdir -p $(RUN_DIR)
	@$(MAKE) --no-print-directory _start-proc NAME=api
	@$(MAKE) --no-print-directory _start-proc NAME=worker
	@$(MAKE) --no-print-directory status-app

down-app:
	@$(MAKE) --no-print-directory _stop-proc NAME=api
	@$(MAKE) --no-print-directory _stop-proc NAME=worker

restart-app: down-app up-app

status-app:
	@for svc in api worker; do \
		pidfile="$(RUN_DIR)/$$svc.pid"; \
		if [ -f "$$pidfile" ] && kill -0 $$(cat "$$pidfile") 2>/dev/null; then \
			echo "$$svc: running (pid $$(cat $$pidfile), log $(RUN_DIR)/$$svc.log)"; \
		else \
			echo "$$svc: stopped"; \
		fi; \
	done

logs-api:
	tail -f $(RUN_DIR)/api.log

logs-worker:
	tail -f $(RUN_DIR)/worker.log

# Bring up everything a dev needs in one shot: compose deps + app binaries.
# Waits for compose deps to report healthy before starting the app (the compose
# up command returns once containers are *started*, not once they're *healthy*).
# down-all leaves compose containers intact (use `make down` to remove them).
up-all: up _wait-deps up-app

down-all: down-app
	docker compose stop

# _wait-deps blocks until the three required compose services report healthy.
# Used by up-all so it can be a single shot instead of up + sleep + up-app.
_wait-deps:
	@echo "waiting for compose services to be healthy..."
	@for i in $$(seq 1 30); do \
		all_healthy=true; \
		for svc in postgres redis clickhouse; do \
			status=$$(docker inspect --format='{{.State.Health.Status}}' url-shortener-$$svc-1 2>/dev/null || echo "missing"); \
			if [ "$$status" != "healthy" ]; then all_healthy=false; break; fi; \
		done; \
		if [ "$$all_healthy" = "true" ]; then \
			echo "all deps healthy"; \
			exit 0; \
		fi; \
		sleep 1; \
	done; \
	echo "ERROR: compose services did not become healthy within 30s"; \
	exit 1

# --- helpers (not part of the public target surface) ---

# _start-proc starts ./bin/$(NAME) if not already running. Idempotent.
_start-proc:
	@pidfile="$(RUN_DIR)/$(NAME).pid"; \
	logfile="$(RUN_DIR)/$(NAME).log"; \
	if [ -f "$$pidfile" ] && kill -0 $$(cat "$$pidfile") 2>/dev/null; then \
		echo "$(NAME): already running (pid $$(cat $$pidfile))"; \
		exit 0; \
	fi; \
	rm -f "$$pidfile"; \
	nohup ./bin/$(NAME) > "$$logfile" 2>&1 & echo $$! > "$$pidfile"; \
	echo "$(NAME): started (pid $$(cat $$pidfile), log $$logfile)"

# _stop-proc sends SIGTERM and waits up to 15s for the process to exit.
_stop-proc:
	@pidfile="$(RUN_DIR)/$(NAME).pid"; \
	if [ ! -f "$$pidfile" ]; then \
		echo "$(NAME): no pid file, skipping"; \
		exit 0; \
	fi; \
	pid=$$(cat "$$pidfile"); \
	if ! kill -0 $$pid 2>/dev/null; then \
		echo "$(NAME): stale pid file (pid $$pid not running), removing"; \
		rm -f "$$pidfile"; \
		exit 0; \
	fi; \
	kill -TERM $$pid; \
	for i in $$(seq 1 15); do \
		if ! kill -0 $$pid 2>/dev/null; then \
			echo "$(NAME): stopped (pid $$pid)"; \
			rm -f "$$pidfile"; \
			exit 0; \
		fi; \
		sleep 1; \
	done; \
	echo "$(NAME): did not exit within 15s, SIGKILL"; \
	kill -KILL $$pid 2>/dev/null; \
	rm -f "$$pidfile"

# check-deps verifies that compose services are healthy before starting the
# app. Fails loudly with a useful message rather than letting the binary
# crash on its startup ping.
check-deps:
	@for svc in postgres redis clickhouse; do \
		status=$$(docker inspect --format='{{.State.Health.Status}}' url-shortener-$$svc-1 2>/dev/null || echo "missing"); \
		if [ "$$status" != "healthy" ]; then \
			echo "ERROR: compose service '$$svc' is '$$status' — run 'make up' and wait for deps to be healthy."; \
			exit 1; \
		fi; \
	done
