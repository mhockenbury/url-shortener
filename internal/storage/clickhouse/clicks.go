// Package clickhouse holds the ClickHouse-backed adapter for raw click events.
// The worker calls Insert in batches; the stats handler calls Stats.
//
// ClickHouse prefers large batch inserts (thousands of rows) — the worker's
// batch policy (every 1s or N rows) is the right shape. Per-row inserts work
// but waste compression and merge efficiency.
package clickhouse

import (
	"context"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// ClickEvent is the row shape written to the `clicks` table. Field names
// mirror the schema in migrations/clickhouse/0001_init.sql.
type ClickEvent struct {
	ShortCode string
	Timestamp time.Time
	Referrer  string
	UserAgent string
}

// HourlyBucket is one row of the stats response — count of clicks in a
// given hour. Returned in chronological order by Stats.
type HourlyBucket struct {
	Hour  time.Time
	Count uint64
}

// StatsResult is what Stats returns for a single short_code.
type StatsResult struct {
	Total   uint64
	Hourly  []HourlyBucket
}

// Client wraps a clickhouse-go connection. Constructed via NewClient; the
// caller owns lifecycle (Close when done).
type Client struct {
	conn driver.Conn
}

// Config captures the minimal fields needed to connect. The native protocol
// (port 9000) is used rather than HTTP (8123) for throughput on inserts.
type Config struct {
	Addr     string // host:port, e.g. "localhost:9000"
	Database string
	Username string
	Password string

	// DialTimeout bounds the initial connect. Zero means a reasonable default.
	DialTimeout time.Duration
}

// NewClient opens a connection pool and pings once to fail fast on
// misconfiguration.
func NewClient(ctx context.Context, cfg Config) (*Client, error) {
	if cfg.DialTimeout == 0 {
		cfg.DialTimeout = 5 * time.Second
	}

	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{cfg.Addr},
		Auth: clickhouse.Auth{
			Database: cfg.Database,
			Username: cfg.Username,
			Password: cfg.Password,
		},
		DialTimeout: cfg.DialTimeout,
		// Compression reduces insert bandwidth at the cost of CPU — reasonable
		// default for a batch-insert workload.
		Compression: &clickhouse.Compression{Method: clickhouse.CompressionLZ4},
	})
	if err != nil {
		return nil, fmt.Errorf("clickhouse.Open: %w", err)
	}
	if err := conn.Ping(ctx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("clickhouse ping: %w", err)
	}
	return &Client{conn: conn}, nil
}

// Close closes the underlying connection pool.
func (c *Client) Close() error {
	return c.conn.Close()
}

// Ping pings ClickHouse. Used by /healthz.
func (c *Client) Ping(ctx context.Context) error {
	return c.conn.Ping(ctx)
}

// Insert batch-writes click events. Uses PrepareBatch + Append + Send, which
// is the efficient path clickhouse-go offers — all rows ship in a single
// network round-trip as a columnar block.
//
// An empty batch is a no-op (returns nil). Timestamps must be non-zero;
// callers that omit them should default to time.Now() before calling.
func (c *Client) Insert(ctx context.Context, events []ClickEvent) error {
	if len(events) == 0 {
		return nil
	}

	batch, err := c.conn.PrepareBatch(ctx, "INSERT INTO clicks (short_code, ts, referrer, user_agent)")
	if err != nil {
		return fmt.Errorf("prepare batch: %w", err)
	}

	for i := range events {
		ev := events[i]
		ts := ev.Timestamp
		if ts.IsZero() {
			// Defensive — worker should have populated this. Prefer now over
			// rejecting the batch and losing everything.
			ts = time.Now().UTC()
		}
		if err := batch.Append(ev.ShortCode, ts.UTC(), ev.Referrer, ev.UserAgent); err != nil {
			// Abort the whole batch on first append error. clickhouse-go
			// aborts anyway — be explicit rather than silently partial.
			_ = batch.Abort()
			return fmt.Errorf("append row %d: %w", i, err)
		}
	}
	if err := batch.Send(); err != nil {
		return fmt.Errorf("send batch: %w", err)
	}
	return nil
}

// Stats returns per-hour click counts for shortCode in [since, until).
// Ordered ascending by hour. since must be <= until; otherwise an error
// is returned rather than an empty-but-confusing result.
//
// The query uses toStartOfHour(ts) to bucket; this is a tight primary-key
// range scan thanks to ORDER BY (short_code, ts) on the MergeTree.
func (c *Client) Stats(ctx context.Context, shortCode string, since, until time.Time) (StatsResult, error) {
	if since.After(until) {
		return StatsResult{}, fmt.Errorf("stats: since (%s) after until (%s)", since, until)
	}
	const q = `
        SELECT toStartOfHour(ts) AS hour, count() AS clicks
        FROM clicks
        WHERE short_code = ?
          AND ts >= ?
          AND ts <  ?
        GROUP BY hour
        ORDER BY hour`

	rows, err := c.conn.Query(ctx, q, shortCode, since.UTC(), until.UTC())
	if err != nil {
		return StatsResult{}, fmt.Errorf("stats query: %w", err)
	}
	defer rows.Close()

	var out StatsResult
	for rows.Next() {
		var bucket HourlyBucket
		if err := rows.Scan(&bucket.Hour, &bucket.Count); err != nil {
			return StatsResult{}, fmt.Errorf("stats scan: %w", err)
		}
		out.Hourly = append(out.Hourly, bucket)
		out.Total += bucket.Count
	}
	if err := rows.Err(); err != nil {
		return StatsResult{}, fmt.Errorf("stats rows err: %w", err)
	}
	return out, nil
}
