package clickhouse_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	ch "github.com/mhockenbury/url-shortener/internal/storage/clickhouse"
)

// Integration tests against docker-compose ClickHouse. Skipped if
// CLICKHOUSE_ADDR (or the default) isn't reachable.
//
// Run locally:
//   docker compose up -d clickhouse
//   docker compose exec -T clickhouse clickhouse-client --user shortener \
//       --password shortener --database shortener < migrations/clickhouse/0001_init.sql
//   go test ./internal/storage/clickhouse/
//
// Isolation: each test uses a unique short_code derived from t.Name() +
// nanos, so concurrent runs and prior-run residue don't interfere.

const (
	defaultAddr = "localhost:9000"
	defaultDB   = "shortener"
	defaultUser = "shortener"
	defaultPass = "shortener"
)

func testClient(t *testing.T) *ch.Client {
	t.Helper()

	addr := os.Getenv("CLICKHOUSE_ADDR")
	if addr == "" {
		addr = defaultAddr
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	client, err := ch.NewClient(ctx, ch.Config{
		Addr:     addr,
		Database: defaultDB,
		Username: defaultUser,
		Password: defaultPass,
	})
	if err != nil {
		t.Skipf("clickhouse not reachable (addr=%s): %v", addr, err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func uniqueCode(t *testing.T, prefix string) string {
	return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
}

func TestClient_InsertEmptyIsNoOp(t *testing.T) {
	c := testClient(t)
	if err := c.Insert(context.Background(), nil); err != nil {
		t.Errorf("Insert(nil): %v", err)
	}
	if err := c.Insert(context.Background(), []ch.ClickEvent{}); err != nil {
		t.Errorf("Insert([]): %v", err)
	}
}

func TestClient_InsertAndStatsRoundTrip(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	code := uniqueCode(t, "rt")

	// Seed 3 events in hour H, 2 in H+1. Pick a base far enough in the past
	// that there's no overlap with "now" queries.
	base := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Hour)
	events := []ch.ClickEvent{
		{ShortCode: code, Timestamp: base.Add(5 * time.Minute)},
		{ShortCode: code, Timestamp: base.Add(10 * time.Minute), UserAgent: "ua1"},
		{ShortCode: code, Timestamp: base.Add(30 * time.Minute), Referrer: "https://ref.example"},
		{ShortCode: code, Timestamp: base.Add(65 * time.Minute)},
		{ShortCode: code, Timestamp: base.Add(90 * time.Minute)},
	}
	if err := c.Insert(ctx, events); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	res, err := c.Stats(ctx, code, base.Add(-time.Minute), base.Add(3*time.Hour))
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if res.Total != 5 {
		t.Errorf("Total = %d, want 5", res.Total)
	}
	if len(res.Hourly) != 2 {
		t.Fatalf("Hourly buckets = %d, want 2. got %+v", len(res.Hourly), res.Hourly)
	}
	if res.Hourly[0].Count != 3 {
		t.Errorf("bucket[0].Count = %d, want 3", res.Hourly[0].Count)
	}
	if res.Hourly[1].Count != 2 {
		t.Errorf("bucket[1].Count = %d, want 2", res.Hourly[1].Count)
	}
	if !res.Hourly[0].Hour.Equal(base) {
		t.Errorf("bucket[0].Hour = %v, want %v", res.Hourly[0].Hour, base)
	}
	if !res.Hourly[1].Hour.Equal(base.Add(time.Hour)) {
		t.Errorf("bucket[1].Hour = %v, want %v", res.Hourly[1].Hour, base.Add(time.Hour))
	}
}

func TestClient_StatsEmptyForUnknownCode(t *testing.T) {
	c := testClient(t)
	res, err := c.Stats(context.Background(),
		uniqueCode(t, "nobody"),
		time.Now().Add(-24*time.Hour),
		time.Now())
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if res.Total != 0 || len(res.Hourly) != 0 {
		t.Errorf("unknown code should return empty; got %+v", res)
	}
}

func TestClient_StatsFiltersOutsideRange(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	code := uniqueCode(t, "range")

	base := time.Now().Add(-5 * time.Hour).UTC().Truncate(time.Hour)
	events := []ch.ClickEvent{
		{ShortCode: code, Timestamp: base.Add(-2 * time.Hour)}, // before window
		{ShortCode: code, Timestamp: base.Add(30 * time.Minute)},
		{ShortCode: code, Timestamp: base.Add(time.Hour)},
		{ShortCode: code, Timestamp: base.Add(10 * time.Hour)}, // after window
	}
	if err := c.Insert(ctx, events); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	// Window: [base, base+2h) — should include rows 2 and 3 only.
	res, err := c.Stats(ctx, code, base, base.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if res.Total != 2 {
		t.Errorf("Total = %d, want 2", res.Total)
	}
}

func TestClient_StatsRejectsInvertedRange(t *testing.T) {
	c := testClient(t)
	now := time.Now()
	_, err := c.Stats(context.Background(), "x", now, now.Add(-time.Hour))
	if err == nil {
		t.Errorf("inverted range should error")
	}
}

func TestClient_InsertLargeBatch(t *testing.T) {
	// Proves the batch path scales. 10k rows is still small for ClickHouse
	// but exercises the append/send loop under more than toy load.
	c := testClient(t)
	ctx := context.Background()
	code := uniqueCode(t, "big")

	const n = 10_000
	base := time.Now().Add(-3 * time.Hour).UTC()
	events := make([]ch.ClickEvent, n)
	for i := 0; i < n; i++ {
		events[i] = ch.ClickEvent{
			ShortCode: code,
			Timestamp: base.Add(time.Duration(i) * time.Millisecond),
		}
	}

	start := time.Now()
	if err := c.Insert(ctx, events); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	insertDur := time.Since(start)
	t.Logf("inserted %d rows in %s (%.0f rows/s)", n, insertDur, float64(n)/insertDur.Seconds())

	res, err := c.Stats(ctx, code, base.Add(-time.Hour), base.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if res.Total != n {
		t.Errorf("Total = %d, want %d", res.Total, n)
	}
}

func TestClient_InsertZeroTimestampDefaultsToNow(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	code := uniqueCode(t, "zerots")

	before := time.Now().Add(-time.Second)
	if err := c.Insert(ctx, []ch.ClickEvent{{ShortCode: code}}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	after := time.Now().Add(time.Second)

	res, err := c.Stats(ctx, code, before, after)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if res.Total != 1 {
		t.Errorf("Total = %d, want 1 (zero-ts should have defaulted to now)", res.Total)
	}
}
