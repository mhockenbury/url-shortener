# Tradeoffs — url-shortener

The four core decisions live in the README §6. This doc is where we capture additional smaller tradeoffs that come up during implementation, so the README stays focused on the big ones.

## Pending / to be captured during build

- Redirect response: 301 (permanent) vs 302 (temporary). 301 is browser-cached aggressively — good for performance, bad for analytics accuracy. Likely going with 302 to keep per-click observability, but worth benchmarking.
- Redis cache key format: raw `short_code` vs prefixed (`url:{code}`). Prefix enables multi-tenant reuse of the same Redis later.
- **URL validation strictness.** `POST /shorten` needs input validation beyond "is it a parseable URL." Plan for the initial handler:
  - *Must:* scheme is `http` or `https` only (reject `javascript:`, `data:`, `file:`, `ftp:`, etc. — closes the open-redirect-to-XSS vector)
  - *Must:* length cap (~2048 chars matching §1 requirements)
  - *Should:* reject hosts that resolve to RFC1918 / loopback / link-local / multicast (SSRF guard — without this, someone can shorten `http://localhost:6379/` or `http://169.254.169.254/` and weaponize the redirect)
  - *Skip for the lab:* Safe Browsing API lookups, blocklists, CAPTCHA, user quotas, takedown workflows. These are real production concerns but out of scope here.
  Implement as a `ValidateTarget(rawURL) error` helper in the HTTP package so the rules are in one place and unit-testable without a network round trip (mock the resolver or skip DNS in tests).
- Connection pool sizing (pgx, redis, clickhouse): initial guesses, refined after first benchmark pass.
- Goose vs plain psql for migrations: plain `.sql` + a `make migrate` wrapper is enough for the lab; goose only if we need down-migrations.
- ClickHouse insert batch size: 1000 rows / 1s is the starting heuristic. ClickHouse prefers larger batches (10k+) for best compression and merge behavior — revisit after first benchmark pass. Trade-off is batch size vs end-to-end analytics latency.
- ClickHouse: raw `MergeTree` vs `ReplacingMergeTree` for dedup. Chose raw — at-least-once dupes accepted as overcount. Revisit if stats accuracy becomes a requirement.
- ClickHouse: when to add a materialized view for hourly rollups. Not needed at current scale; noted as a V2 lever if `GET /stats/:code` latency becomes a problem.
- **OpenAPI spec generation.** Plan: spec must be **generated from code**, not hand-maintained — hand-written OpenAPI drifts from real handler behavior the moment anyone forgets to update it. Candidates:
  - *`swaggo/swag`* — scans handler godoc comments and emits OpenAPI 2.0. Low friction, works with any router including chi, but the source of truth is *comments*, which can themselves drift from the real handler code.
  - *`ogen`* — OpenAPI 3 generated from Go types via struct tags. Types are the source of truth so drift is less likely, but feels heavier; typically used spec-first.
  - *Custom `DescribeOpenAPI(router) → openapi.yaml`* — walk our chi router + request/response Go types, emit a spec. Most control, most code to maintain.
  - *`huma`* / *`goa`* — frameworks that emit OpenAPI as a side effect of route declaration. Means swapping our router, which we don't want to do now.

  Starting point: `swaggo/swag` for speed, with a CI check (`make openapi && git diff --exit-code docs/openapi.yaml`) so generated spec must be committed and stays current. Revisit if comment-drift bites us.
- **Expiry enforcement location.** Currently pushed to the HTTP handler (compare `ExpiresAt` to `now()`, return 410). Alternative: enforce in SQL (`WHERE expires_at IS NULL OR expires_at > now()`). Tradeoff: handler-side lets the cache serve briefly-stale-expired rows without extra invalidation machinery, and puts all 410-vs-301 logic in one place. SQL-side makes the contract explicit at the storage boundary and avoids ever returning an expired `Link` to callers. Revisit once the cache layer exists and we can reason about the full invalidation story.
- **Thundering herd on cache miss.** When a popular short_code's cache entry expires (TTL) or is evicted, concurrent redirects can stampede the DB with identical `SELECT ... WHERE short_code=$1` queries. Plan: wrap the cache-miss fallback in `golang.org/x/sync/singleflight` keyed by `short_code`, so only one goroutine fetches from Postgres while the rest wait on its result. Adds a few lines to the redirect handler; caps DB load under cache eviction of hot keys. Apply when wiring up the HTTP handler.
- **Sequential-ID enumeration.** The counter+base62 strategy produces IDs that are trivially walkable: given one valid short_code, an attacker can decode it, increment, and re-encode to probe neighboring links — effectively scraping the dataset. Mitigations in order of complexity:
  1. *Accept it* — the lab is not modeling a system where link privacy is a security guarantee. This is the current stance.
  2. *Non-sequential encoding* — run `id` through a bijective shuffle (e.g., Feistel network or Hashids) before base62. Keeps short codes short, adds no DB round-trip, but complicates debugging.
  3. *Random slug with retry on collision* — breaks enumeration entirely; trades write-path DB round-trips for unpredictability.
  4. *Snowflake-style IDs* — mentioned in README §6 for multi-region, also helps enumeration as a side effect.
  Revisit if the system ever serves non-public links or a privacy-sensitive deployment.

## Resolved

<!-- Move decisions here as they solidify during implementation. Format: context → choice → why. -->
