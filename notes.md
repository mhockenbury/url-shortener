# url-shortener — Notes

Subproject-local notes: decisions made during implementation, commands, references. The README is the design doc; this file is the working log.

## Decisions made during scaffolding

- **Go module path:** `github.com/mhockenbury/url-shortener`
- **Migrations:** plain `.sql` files for now; `make migrate` not yet wired. Postgres migrations in `migrations/`, ClickHouse in `migrations/clickhouse/`. Will reach for goose only if we need down-migrations
- **API + worker run on host** during dev; compose runs Postgres + Redis + ClickHouse. Dockerizing API/worker is deferred until benchmarking phase to avoid container overhead noise in measurements
- **Redirect status code:** chose 302 so browsers re-hit on each click (keeps analytics accurate). Revisit with real browser-cache benchmarks if ever needed.
- **Analytics store:** ClickHouse, raw events, no aggregation at write time. Chosen over Postgres-aggregates for correctness-of-shape and the educational value of exercising a real columnar store — see README §6
- **Go version bumped 1.22 → 1.25** when adding `pgx/v5 v5.9.2` (pgx latest requires 1.25). Go's transparent toolchain management downloaded 1.25 automatically. System install is still 1.22.5; the `go` tool will fetch 1.25 on demand for this module. Fine for the lab; worth noting if we ever share the repo and downstream users need to match.

## Decisions made during implementation

- **Service interface lives in `internal/http`, not `internal/shortener`.** Go idiom: define interfaces at the consumer. Lets handler tests fake the service without pulling pgx/redis transitively.
- **Click emission on a detached-context goroutine.** The redirect returns before the click is written; a cancelled request (closed connection) doesn't drop the in-flight event. 2s timeout bounds runaway goroutines.
- **Singleflight on cache miss, keyed by short_code.** Collapses concurrent DB fallbacks on the same hot expired code into one query. Thundering-herd mitigation.
- **Cache TTL capped at remaining row lifetime.** Prevents caching a row past its `expires_at` and serving a stale-but-otherwise-valid 302 for an expired link beyond the invalidation window.
- **Literal-IP SSRF check runs even with a nil resolver.** Test-driven fix: a nil resolver (used to skip DNS in unit tests) was letting `http://127.0.0.1/` through. Fix was on the implementation side, not the test.
- **`InsertAlias` kept separate from `Insert`** even though they share SQL today. Future-proof for auth / permission checks / reserved-alias ID range.
- **Integration tests hit real Postgres + Redis via docker-compose.** Skip cleanly when unreachable (env-based DSN/addr with sensible defaults). Per matt's feedback memory: no mocked DBs.
- **Per-test allocator-row naming uses `time.Now().UnixNano()` for both the name and the starting `next_id`** so parallel runs on a shared `links` table don't collide with leftover rows from prior runs.
- **`go run` swallows signals.** Graceful-shutdown testing must use the compiled binary (`./bin/api`) — `go run ./cmd/api` + `kill -TERM` exits the child ungracefully. Worth remembering across Go subprojects.

## Open questions (subproject-local)

- Goose vs plain psql runner for migrations
- k6 vs vegeta for load tests
- Should `POST /shorten` be idempotent (same URL → same code) or always mint a new one? Currently "always new" because counter strategy makes idempotence expensive (would need a separate `long_url → short_code` lookup table). Not blocking.
- When to introduce structured error types beyond the typed sentinels (e.g., wrapping context into a `*AppError` for handler-side mapping). Currently handlers use `errors.Is` + switch — fine at this size.
- **OpenAPI generator choice** — `swaggo/swag` (godoc-comment-driven) vs `ogen` (Go-types-driven) vs custom. Leaning `swag` for speed with a CI drift check. See `docs/tradeoffs.md`. Must be code-generated, never hand-written.

## Commands & Snippets

```bash
# Bring up dependencies
docker compose up -d postgres redis clickhouse

# Apply migrations (one-time per env, manual for now)
docker compose exec -T postgres psql -U shortener -d shortener < migrations/0001_init.sql
docker compose exec -T clickhouse clickhouse-client --user shortener --password shortener --database shortener < migrations/clickhouse/0001_init.sql

# Run the API (compiled binary — needed for graceful shutdown to work)
go build -o bin/api ./cmd/api && ./bin/api

# Or dev loop via Makefile (note: `go run` does not forward SIGTERM cleanly)
make run-api       # api on :8080
make run-worker    # worker (not yet implemented)

# Full test suite (skips integration tests if deps unreachable)
go test ./...

# Smoke the API
curl -s -i localhost:8080/healthz
curl -s -X POST localhost:8080/shorten -H 'content-type: application/json' \
  -d '{"url":"https://example.com/hello"}'
curl -s -i localhost:8080/<code>                # 302 redirect
curl -s -i localhost:8080/stats/<code>          # 501 until ClickHouse wired

# Inspect id_allocator
docker compose exec postgres psql -U shortener -d shortener -c 'SELECT * FROM id_allocator;'

# Watch the clicks stream
docker compose exec redis redis-cli XLEN clicks
docker compose exec redis redis-cli XREVRANGE clicks + - COUNT 10

# ClickHouse — top codes (once worker is wired)
docker compose exec clickhouse clickhouse-client --user shortener --password shortener --database shortener \
    --query "SELECT short_code, count() FROM clicks GROUP BY short_code ORDER BY count() DESC LIMIT 10"

# ClickHouse — hourly series for one code
docker compose exec clickhouse clickhouse-client --user shortener --password shortener --database shortener \
    --query "SELECT toStartOfHour(ts) AS h, count() FROM clicks WHERE short_code = 'abc' GROUP BY h ORDER BY h"
```

## Env vars (cmd/api)

| Var | Default | Notes |
|-----|---------|-------|
| `DATABASE_URL` | `postgres://shortener:shortener@localhost:5432/shortener` | Password redacted in logs |
| `REDIS_ADDR` | `localhost:6379` | |
| `HTTP_ADDR` | `:8080` | |
| `BASE_URL` | `http://localhost:8080` | Prefixed onto `short_url` in responses |
| `ID_ALLOCATOR_NAME` | `links` | Lets multiple API instances share a Postgres safely by using distinct rows |
| `ID_BATCH_SIZE` | `1000` | IDs reserved per DB round-trip |
| `CACHE_TTL` | `1h` | Redis TTL on `url:<code>` entries; capped at remaining row lifetime |
| `SHUTDOWN_GRACE` | `15s` | Max time to drain on SIGTERM |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |

## References

- README.md — full design doc (requirements through failure modes)
- docs/architecture.md — component deep-dive
- docs/tradeoffs.md — smaller tradeoffs captured during build
- docs/benchmarks.md — planned scenarios
- pgx docs: https://github.com/jackc/pgx
- go-redis docs: https://github.com/redis/go-redis
- clickhouse-go v2 docs: https://github.com/ClickHouse/clickhouse-go
- chi docs: https://github.com/go-chi/chi
- Redis Streams reference: https://redis.io/docs/data-types/streams/
- ClickHouse MergeTree docs: https://clickhouse.com/docs/en/engines/table-engines/mergetree-family/mergetree
- ClickHouse LowCardinality: https://clickhouse.com/docs/en/sql-reference/data-types/lowcardinality
