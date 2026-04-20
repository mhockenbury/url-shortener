# Tradeoffs — url-shortener

The four core decisions live in the README §6. This doc is where we capture additional smaller tradeoffs that come up during implementation, so the README stays focused on the big ones.

## Pending / to be captured during build

- Redirect response: 301 (permanent) vs 302 (temporary). 301 is browser-cached aggressively — good for performance, bad for analytics accuracy. Likely going with 302 to keep per-click observability, but worth benchmarking.
- Redis cache key format: raw `short_code` vs prefixed (`url:{code}`). Prefix enables multi-tenant reuse of the same Redis later.
- URL validation strictness: parse + scheme check only, or also DNS resolution / length caps / SSRF-adjacent block rules? Starting permissive.
- Connection pool sizing (pgx, redis, clickhouse): initial guesses, refined after first benchmark pass.
- Goose vs plain psql for migrations: plain `.sql` + a `make migrate` wrapper is enough for the lab; goose only if we need down-migrations.
- ClickHouse insert batch size: 1000 rows / 1s is the starting heuristic. ClickHouse prefers larger batches (10k+) for best compression and merge behavior — revisit after first benchmark pass. Trade-off is batch size vs end-to-end analytics latency.
- ClickHouse: raw `MergeTree` vs `ReplacingMergeTree` for dedup. Chose raw — at-least-once dupes accepted as overcount. Revisit if stats accuracy becomes a requirement.
- ClickHouse: when to add a materialized view for hourly rollups. Not needed at current scale; noted as a V2 lever if `GET /stats/:code` latency becomes a problem.

## Resolved

<!-- Move decisions here as they solidify during implementation. Format: context → choice → why. -->
