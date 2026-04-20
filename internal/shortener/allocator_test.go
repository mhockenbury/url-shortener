package shortener_test

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/matt/url-shortener/internal/shortener"
)

// Integration tests hit a real Postgres brought up by docker-compose. They're
// skipped if DATABASE_URL isn't set or the DB isn't reachable, so `go test
// ./...` on a fresh checkout without compose running still passes.
//
// To run locally:
//   docker-compose up -d postgres
//   docker-compose exec -T postgres psql -U shortener -d shortener \
//       < migrations/0001_init.sql
//   DATABASE_URL=postgres://shortener:shortener@localhost:5432/shortener go test ./internal/shortener/ -run Allocator

const defaultTestDSN = "postgres://shortener:shortener@localhost:5432/shortener"

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = defaultTestDSN
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Skipf("postgres not reachable (dsn=%s): %v", dsn, err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("postgres ping failed (dsn=%s): %v", dsn, err)
	}
	return pool
}

// resetAllocator seeds a unique allocator row per test so concurrent test
// runs don't stomp on each other. Uses the test name as the allocator key.
func resetAllocator(t *testing.T, pool *pgxpool.Pool, name string) {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx,
		`INSERT INTO id_allocator (name, next_id) VALUES ($1, 1)
		 ON CONFLICT (name) DO UPDATE SET next_id = 1`, name)
	if err != nil {
		t.Fatalf("reset allocator row %q: %v", name, err)
	}
}

func TestAllocator_Sequential(t *testing.T) {
	pool := testPool(t)
	defer pool.Close()

	name := "test_sequential"
	resetAllocator(t, pool, name)

	a, err := shortener.NewAllocator(pool, name, 10)
	if err != nil {
		t.Fatalf("NewAllocator: %v", err)
	}

	ctx := context.Background()
	for want := uint64(1); want <= 25; want++ {
		got, err := a.Next(ctx)
		if err != nil {
			t.Fatalf("Next at iteration %d: %v", want, err)
		}
		if got != want {
			t.Fatalf("Next() = %d, want %d", got, want)
		}
	}
}

func TestAllocator_RefillsExactlyWhenNeeded(t *testing.T) {
	pool := testPool(t)
	defer pool.Close()

	name := "test_refill"
	resetAllocator(t, pool, name)

	a, err := shortener.NewAllocator(pool, name, 5)
	if err != nil {
		t.Fatalf("NewAllocator: %v", err)
	}

	ctx := context.Background()
	// Exhaust the first batch.
	for i := 0; i < 5; i++ {
		if _, err := a.Next(ctx); err != nil {
			t.Fatalf("Next %d: %v", i, err)
		}
	}
	if got := a.Remaining(); got != 0 {
		t.Fatalf("Remaining after first batch = %d, want 0", got)
	}

	// The DB row should have advanced by exactly one batch.
	var nextID uint64
	if err := pool.QueryRow(ctx,
		`SELECT next_id FROM id_allocator WHERE name=$1`, name).Scan(&nextID); err != nil {
		t.Fatalf("read allocator row: %v", err)
	}
	if nextID != 6 {
		t.Fatalf("DB next_id = %d, want 6 after consuming batch of 5", nextID)
	}

	// One more call triggers a refill and returns id 6.
	got, err := a.Next(ctx)
	if err != nil {
		t.Fatalf("Next after refill: %v", err)
	}
	if got != 6 {
		t.Fatalf("Next() after refill = %d, want 6", got)
	}
}

func TestAllocator_Concurrent_NoDuplicates(t *testing.T) {
	pool := testPool(t)
	defer pool.Close()

	name := "test_concurrent"
	resetAllocator(t, pool, name)

	a, err := shortener.NewAllocator(pool, name, 100)
	if err != nil {
		t.Fatalf("NewAllocator: %v", err)
	}

	const workers = 16
	const perWorker = 500
	total := workers * perWorker

	ctx := context.Background()
	ch := make(chan uint64, total)

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				id, err := a.Next(ctx)
				if err != nil {
					t.Errorf("Next: %v", err)
					return
				}
				ch <- id
			}
		}()
	}
	wg.Wait()
	close(ch)

	seen := make(map[uint64]struct{}, total)
	var min, max uint64 = ^uint64(0), 0
	for id := range ch {
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate ID handed out: %d", id)
		}
		seen[id] = struct{}{}
		if id < min {
			min = id
		}
		if id > max {
			max = id
		}
	}
	if len(seen) != total {
		t.Fatalf("distinct IDs = %d, want %d", len(seen), total)
	}
	// IDs are contiguous from 1 because only this allocator is using the row.
	if min != 1 || max != uint64(total) {
		t.Fatalf("range = [%d, %d], want [1, %d]", min, max, total)
	}
}

func TestAllocator_SeparateInstancesShareRow(t *testing.T) {
	// Two allocator instances bound to the same DB row should produce
	// disjoint ID sets. Each grabs its own reserved range.
	pool := testPool(t)
	defer pool.Close()

	name := "test_two_instances"
	resetAllocator(t, pool, name)

	a1, err := shortener.NewAllocator(pool, name, 10)
	if err != nil {
		t.Fatalf("NewAllocator a1: %v", err)
	}
	a2, err := shortener.NewAllocator(pool, name, 10)
	if err != nil {
		t.Fatalf("NewAllocator a2: %v", err)
	}

	ctx := context.Background()
	seen := make(map[uint64]struct{})
	take := func(a *shortener.Allocator, n int) {
		for i := 0; i < n; i++ {
			id, err := a.Next(ctx)
			if err != nil {
				t.Fatalf("Next: %v", err)
			}
			if _, dup := seen[id]; dup {
				t.Fatalf("duplicate ID across instances: %d", id)
			}
			seen[id] = struct{}{}
		}
	}

	// Interleave: a1 takes 5, a2 takes 5, a1 takes 10 (forces refill), a2 takes 10.
	take(a1, 5)
	take(a2, 5)
	take(a1, 10)
	take(a2, 10)

	if len(seen) != 30 {
		t.Fatalf("distinct IDs = %d, want 30", len(seen))
	}
}

func TestAllocator_ConstructorValidation(t *testing.T) {
	cases := []struct {
		name      string
		pool      *pgxpool.Pool
		allocName string
		batch     uint64
	}{
		{"nil pool", nil, "x", 10},
		{"empty name", &pgxpool.Pool{}, "", 10},
		{"zero batch", &pgxpool.Pool{}, "x", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := shortener.NewAllocator(c.pool, c.allocName, c.batch); err == nil {
				t.Fatalf("want error, got nil")
			}
		})
	}
}
