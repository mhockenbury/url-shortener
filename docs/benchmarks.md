# Benchmarks — url-shortener

## Tooling

**k6** (v1.7.1). Chosen over vegeta because multi-stage scenarios like
cache-hit-vs-miss and allocator-contention sweeps are expressible as first-
class scenarios in k6's JS config, and k6's `thresholds:` block doubles as
an SLO assertion layer that fails the run on regression.

Scripts live under `scripts/k6/`, one per scenario. Invoke via the Makefile:

```bash
make build && ./bin/api     # compose deps must already be up
make load-redirect-hit
```

Overrides (per run) via env: `BASE_URL`, `RPS`, `DURATION`, plus any scenario-
specific knobs documented at the top of each script.

## Scenarios

### 1. Redirect under cache-hit load — `01_redirect_cache_hit.js`

Sustain RPS against a small pre-created + pre-warmed pool of short codes so
every request is a cache hit. Measures the hot-path ceiling.

- **Target:** p99 < 50ms, p95 < 20ms at 1000 RPS on localhost (encoded as k6 thresholds)
- **Pool size:** 20 codes by default (override with `POOL_SIZE`)
- **Follow redirects:** off — we measure the 302 response itself, not the trip to example.com

### 2. Redirect cold start (cache empty) — *not yet written*

Measure how quickly hit rate recovers and what p99 looks like during the cold
window after a Redis flush or a fresh restart.

### 3. Shorten with batch size 1 vs 100 vs 1000 — *not yet written*

Quantify `id_allocator` contention. Expect batched to dominate by an order of
magnitude at concurrency > 10.

### 4. Analytics pipeline end-to-end lag at 2x expected peak — *not yet written*

From redirect to ClickHouse visibility; target < 5s at sustained 2000 events/s.
Measure worker CPU/memory and ClickHouse insert throughput.

### 5. `GET /stats/:code` latency across data volumes — *not yet written*

Seed ClickHouse with 1M, 10M, 100M rows for one code; measure p99 on the
hourly-aggregation query. This is where the columnar-store choice earns its
keep — document the numbers.

### 6. Worker batch-size sweep into ClickHouse — *not yet written*

100 vs 1k vs 10k rows per insert. Measure insert throughput, per-batch latency,
and compressed on-disk size. Informs the production batch-size heuristic.

## Results

### Scenario 1 — Redirect under cache-hit load

**Commit:** d1eb6e1 (base code); results gathered before these benchmarks were committed.
**Hardware:** AMD Ryzen 7 7735HS, Debian 12, single local docker-compose for Postgres + Redis + ClickHouse. API and worker binaries running on the host.
**Workload:** 20-code pool, pre-warmed so every request is a cache hit; 30s measurement window; `redirects: 0` on the client so each 302 is measured, not followed.

| RPS target | Achieved | p50 | p95 | p99 | max | Dropped | Failures |
|------------|----------|-----|-----|-----|-----|---------|----------|
| 1,000 | 997 | 345 µs | 458 µs | 614 µs | 2.08 ms | 0 | 0 / 30 041 |
| 5,000 | 4,965 | 362 µs | 536 µs | — | 176 ms | 421 | 0 / 149 620 |
| 15,000 | 14,791 | 9.78 ms | 39.27 ms | **>50 ms (SLO broken)** | 236 ms | 3 872 | 0 / 446 172 |

**Interpretation:**

- **Headline SLO is comfortably met at design traffic.** At 1k RPS the p99 is **614 µs — roughly 80× under the 50ms target.** The hot path (Redis cache hit + 302) is sub-millisecond end-to-end when the service isn't under saturation pressure.
- **Ceiling lives somewhere between 5k and 15k RPS on this hardware.** At 5k the latency distribution still looks clean (p95 536 µs) but we start seeing occasional 100ms+ outliers and 421 dropped iterations — k6 couldn't sustain the target rate cleanly. At 15k we hit a clear cliff: p95 jumps to 39ms, the threshold fires (k6 exit 99), and 3 872 iterations drop.
- **Zero request failures across all runs.** No 5xx, no request errors — the service fails by queueing/timing out rather than by errors, which is the desired degradation mode.
- **Where the time goes (at design load):** a ~614 µs p99 covers `accept → chi routing → middleware → cache GET (Redis roundtrip over loopback, ~100 µs) → 302 write`. Plausible distribution; matches "read-through cache over loopback" expectations.
- **What this doesn't tell us yet:** real network latency (loopback isn't realistic), cache-miss fallback (scenario 2), contention between redirect + shorten on the shared id_allocator (scenario 3), or anything about the analytics pipeline.

**Reproduce:**
```bash
make build && ./bin/api      # compose deps up, migrations applied
# in another shell:
RPS=1000  DURATION=30s make load-redirect-hit
RPS=5000  DURATION=30s make load-redirect-hit
RPS=15000 DURATION=30s make load-redirect-hit   # expect threshold fail
```

