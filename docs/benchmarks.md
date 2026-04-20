# Benchmarks — url-shortener

Placeholder. Filled in once core implementation is done.

## Planned scenarios

1. **Redirect under cache-hit load.** Target: p99 < 50ms at 1000 RPS on localhost. Measure p50/p99/p999 and throughput ceiling.
2. **Redirect cold start (cache empty).** Measure how quickly hit rate recovers and what p99 looks like during the cold window.
3. **Shorten with batch size 1 vs 100 vs 1000.** Quantify `id_allocator` contention. Expect batched to dominate by an order of magnitude at concurrency > 10.
4. **Analytics pipeline end-to-end lag at 2x expected peak.** From redirect to ClickHouse visibility; target < 5s at sustained 2000 events/s. Measure worker CPU/memory and ClickHouse insert throughput.
5. **`GET /stats/:code` latency across data volumes.** Seed ClickHouse with 1M, 10M, 100M rows for one code; measure p99 on the hourly-aggregation query. This is where the columnar-store choice earns its keep — document the numbers.
6. **Worker batch-size sweep into ClickHouse.** 100 vs 1k vs 10k rows per insert. Measure insert throughput, per-batch latency, and compressed on-disk size. Informs the production batch-size heuristic.

## Tooling

TBD — choosing between k6 (JS-scripted, rich output) and vegeta (Go, simple) at first benchmark. Leaning k6 for the nicer HTML reports that fit the writeup style.

## Results

<!-- Results tables + interpretation, added per scenario. -->
