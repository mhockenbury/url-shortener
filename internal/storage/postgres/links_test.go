package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mhockenbury/url-shortener/internal/storage/postgres"
)

// Integration tests. Skipped if DATABASE_URL (or the default DSN) isn't reachable.
//
// To run locally:
//   docker compose up -d postgres
//   docker compose exec -T postgres psql -U shortener -d shortener \
//       < migrations/0001_init.sql
//   DATABASE_URL=postgres://shortener:shortener@localhost:5432/shortener \
//       go test ./internal/storage/postgres/

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

// uniqueID returns an ID that's very unlikely to collide with other tests or
// prior runs. We use the wall-clock nanosecond timestamp as a test-local
// identifier — not great for production, perfect for isolating test rows.
// The full `links` table gets left alone across test runs; rows are leftovers
// but harmless.
func uniqueID() uint64 {
	return uint64(time.Now().UnixNano())
}

func cleanup(t *testing.T, pool *pgxpool.Pool, ids ...uint64) {
	t.Helper()
	if len(ids) == 0 {
		return
	}
	_, err := pool.Exec(context.Background(),
		`DELETE FROM links WHERE id = ANY($1)`, ids)
	if err != nil {
		t.Logf("cleanup links: %v", err)
	}
}

func TestLinkStore_InsertAndLookup(t *testing.T) {
	pool := testPool(t)
	defer pool.Close()
	store := postgres.NewLinkStore(pool)

	id := uniqueID()
	code := fmt.Sprintf("t%d", id)
	defer cleanup(t, pool, id)

	in := postgres.Link{
		ID:        id,
		ShortCode: code,
		LongURL:   "https://example.com/hello",
	}
	if err := store.Insert(context.Background(), in); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	got, err := store.Lookup(context.Background(), code)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got.ID != in.ID || got.ShortCode != in.ShortCode || got.LongURL != in.LongURL {
		t.Errorf("round trip mismatch: got %+v, in %+v", got, in)
	}
	if got.CreatedAt.IsZero() {
		t.Errorf("CreatedAt should be populated by DB default")
	}
	if got.ExpiresAt != nil {
		t.Errorf("ExpiresAt = %v, want nil", *got.ExpiresAt)
	}
	if got.CreatedBy != nil {
		t.Errorf("CreatedBy = %v, want nil", *got.CreatedBy)
	}
}

func TestLinkStore_InsertWithExpiryAndCreator(t *testing.T) {
	pool := testPool(t)
	defer pool.Close()
	store := postgres.NewLinkStore(pool)

	id := uniqueID()
	code := fmt.Sprintf("t%d", id)
	expires := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Microsecond)
	creator := "tester@example.com"
	defer cleanup(t, pool, id)

	in := postgres.Link{
		ID:        id,
		ShortCode: code,
		LongURL:   "https://example.com/x",
		ExpiresAt: &expires,
		CreatedBy: &creator,
	}
	if err := store.Insert(context.Background(), in); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	got, err := store.Lookup(context.Background(), code)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(expires) {
		t.Errorf("ExpiresAt = %v, want %v", got.ExpiresAt, expires)
	}
	if got.CreatedBy == nil || *got.CreatedBy != creator {
		t.Errorf("CreatedBy = %v, want %q", got.CreatedBy, creator)
	}
}

func TestLinkStore_LookupNotFound(t *testing.T) {
	pool := testPool(t)
	defer pool.Close()
	store := postgres.NewLinkStore(pool)

	_, err := store.Lookup(context.Background(), fmt.Sprintf("missing%d", uniqueID()))
	if !errors.Is(err, postgres.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestLinkStore_DuplicateShortCodeReturnsAliasTaken(t *testing.T) {
	pool := testPool(t)
	defer pool.Close()
	store := postgres.NewLinkStore(pool)

	id1 := uniqueID()
	id2 := id1 + 1
	code := fmt.Sprintf("dup%d", id1)
	defer cleanup(t, pool, id1, id2)

	if err := store.Insert(context.Background(), postgres.Link{
		ID: id1, ShortCode: code, LongURL: "https://example.com/a",
	}); err != nil {
		t.Fatalf("first Insert: %v", err)
	}
	err := store.Insert(context.Background(), postgres.Link{
		ID: id2, ShortCode: code, LongURL: "https://example.com/b",
	})
	if !errors.Is(err, postgres.ErrAliasTaken) {
		t.Fatalf("second Insert err = %v, want ErrAliasTaken", err)
	}
}

func TestLinkStore_DuplicateIDReturnsIDAlreadyTaken(t *testing.T) {
	pool := testPool(t)
	defer pool.Close()
	store := postgres.NewLinkStore(pool)

	id := uniqueID()
	code1 := fmt.Sprintf("a%d", id)
	code2 := fmt.Sprintf("b%d", id)
	defer cleanup(t, pool, id)

	if err := store.Insert(context.Background(), postgres.Link{
		ID: id, ShortCode: code1, LongURL: "https://example.com/a",
	}); err != nil {
		t.Fatalf("first Insert: %v", err)
	}
	err := store.Insert(context.Background(), postgres.Link{
		ID: id, ShortCode: code2, LongURL: "https://example.com/b",
	})
	if !errors.Is(err, postgres.ErrIDAlreadyTaken) {
		t.Fatalf("second Insert err = %v, want ErrIDAlreadyTaken", err)
	}
}

func TestLinkStore_InsertAliasSharesNamespace(t *testing.T) {
	// Custom alias collides with a normally-inserted short_code: should also
	// return ErrAliasTaken. This locks in the "shared namespace" tradeoff
	// documented in README §6.
	pool := testPool(t)
	defer pool.Close()
	store := postgres.NewLinkStore(pool)

	id1 := uniqueID()
	id2 := id1 + 1
	code := fmt.Sprintf("ns%d", id1)
	defer cleanup(t, pool, id1, id2)

	if err := store.Insert(context.Background(), postgres.Link{
		ID: id1, ShortCode: code, LongURL: "https://example.com/regular",
	}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	err := store.InsertAlias(context.Background(), postgres.Link{
		ID: id2, ShortCode: code, LongURL: "https://example.com/alias",
	})
	if !errors.Is(err, postgres.ErrAliasTaken) {
		t.Fatalf("InsertAlias err = %v, want ErrAliasTaken", err)
	}
}
