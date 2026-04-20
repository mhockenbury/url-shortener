-- 0001_init.sql — ClickHouse schema for url-shortener analytics

CREATE TABLE IF NOT EXISTS clicks (
    short_code  LowCardinality(String),
    ts          DateTime64(3),
    referrer    String,
    user_agent  String
) ENGINE = MergeTree
ORDER BY (short_code, ts)
TTL toDateTime(ts) + INTERVAL 90 DAY;
