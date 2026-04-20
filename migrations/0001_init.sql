-- 0001_init.sql — initial Postgres schema for url-shortener
-- ClickHouse schema is in migrations/clickhouse/0001_init.sql

CREATE TABLE IF NOT EXISTS id_allocator (
    name       TEXT PRIMARY KEY,
    next_id    BIGINT NOT NULL
);

INSERT INTO id_allocator (name, next_id)
VALUES ('links', 1)
ON CONFLICT (name) DO NOTHING;

CREATE TABLE IF NOT EXISTS links (
    id          BIGINT PRIMARY KEY,
    short_code  TEXT NOT NULL UNIQUE,
    long_url    TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at  TIMESTAMPTZ,
    created_by  TEXT
);

CREATE INDEX IF NOT EXISTS links_expires_at_idx
    ON links (expires_at)
    WHERE expires_at IS NOT NULL;
