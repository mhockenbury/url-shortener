# url-shortener — Notes

Subproject-local notes: decisions made during implementation, commands, references. The README is the design doc; this file is the working log.

## Decisions made during scaffolding

- **Go module path:** `github.com/matt/url-shortener` — placeholder until the private GitHub repo is created; will be renamed then
- **Migrations:** plain `.sql` files for now; `make migrate` not yet wired. Postgres migrations in `migrations/`, ClickHouse in `migrations/clickhouse/`. Will reach for goose only if we need down-migrations
- **API + worker run on host** during dev; compose runs Postgres + Redis + ClickHouse. Dockerizing API/worker is deferred until benchmarking phase to avoid container overhead noise in measurements
- **Redirect status code:** deferring 301 vs 302 decision to when analytics are wired — see `docs/tradeoffs.md`
- **Analytics store:** ClickHouse, raw events, no aggregation at write time. Chosen over Postgres-aggregates for correctness-of-shape and the educational value of exercising a real columnar store — see README §6

## Open questions (subproject-local)

- Goose vs plain psql runner for migrations
- k6 vs vegeta for load tests
- Should `POST /shorten` be idempotent (same URL → same code) or always mint a new one? Currently planning "always new" because counter strategy makes idempotence expensive (would need a separate `long_url → short_code` lookup table)

## Commands & Snippets

```bash
# Bring up dependencies
docker-compose up -d postgres redis clickhouse

# Apply migrations manually for now
docker-compose exec -T postgres psql -U shortener -d shortener < migrations/0001_init.sql
docker-compose exec -T clickhouse clickhouse-client --user shortener --password shortener --database shortener < migrations/clickhouse/0001_init.sql

# Inspect id_allocator
docker-compose exec postgres psql -U shortener -d shortener -c 'SELECT * FROM id_allocator;'

# Watch the clicks stream
docker-compose exec redis redis-cli XLEN clicks
docker-compose exec redis redis-cli XRANGE clicks - + COUNT 10

# ClickHouse — top codes
docker-compose exec clickhouse clickhouse-client --user shortener --password shortener --database shortener \
    --query "SELECT short_code, count() FROM clicks GROUP BY short_code ORDER BY count() DESC LIMIT 10"

# ClickHouse — hourly series for one code
docker-compose exec clickhouse clickhouse-client --user shortener --password shortener --database shortener \
    --query "SELECT toStartOfHour(ts) AS h, count() FROM clicks WHERE short_code = 'abc' GROUP BY h ORDER BY h"
```

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
