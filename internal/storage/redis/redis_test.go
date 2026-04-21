package redis_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	myredis "github.com/mhockenbury/url-shortener/internal/storage/redis"
)

// Integration tests against docker-compose Redis. Skip if REDIS_ADDR (or the
// default) isn't reachable.
//
// To run locally:
//   docker compose up -d redis
//   REDIS_ADDR=localhost:6379 go test ./internal/storage/redis/

const defaultAddr = "localhost:6379"

func testClient(t *testing.T) *myredis.Client {
	t.Helper()

	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		addr = defaultAddr
	}
	rdb := goredis.NewClient(&goredis.Options{Addr: addr})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		_ = rdb.Close()
		t.Skipf("redis not reachable (addr=%s): %v", addr, err)
	}
	return myredis.NewClient(rdb)
}

func uniqueCode(prefix string) string {
	return fmt.Sprintf("%s%d", prefix, time.Now().UnixNano())
}

func TestCache_RoundTrip(t *testing.T) {
	c := testClient(t)
	defer c.Close()
	ctx := context.Background()

	code := uniqueCode("rt")
	url := "https://example.com/round-trip"

	if err := c.SetLongURL(ctx, code, url, time.Minute); err != nil {
		t.Fatalf("SetLongURL: %v", err)
	}
	got, err := c.GetLongURL(ctx, code)
	if err != nil {
		t.Fatalf("GetLongURL: %v", err)
	}
	if got != url {
		t.Errorf("GetLongURL = %q, want %q", got, url)
	}

	if err := c.DeleteLongURL(ctx, code); err != nil {
		t.Fatalf("DeleteLongURL: %v", err)
	}
}

func TestCache_MissReturnsErrCacheMiss(t *testing.T) {
	c := testClient(t)
	defer c.Close()
	ctx := context.Background()

	code := uniqueCode("miss")
	_, err := c.GetLongURL(ctx, code)
	if !errors.Is(err, myredis.ErrCacheMiss) {
		t.Fatalf("err = %v, want ErrCacheMiss", err)
	}
}

func TestCache_TTLExpires(t *testing.T) {
	c := testClient(t)
	defer c.Close()
	ctx := context.Background()

	code := uniqueCode("ttl")
	if err := c.SetLongURL(ctx, code, "https://example.com/x", 100*time.Millisecond); err != nil {
		t.Fatalf("SetLongURL: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	if _, err := c.GetLongURL(ctx, code); !errors.Is(err, myredis.ErrCacheMiss) {
		t.Fatalf("after TTL: err = %v, want ErrCacheMiss", err)
	}
}

func TestCache_SetRejectsNonPositiveTTL(t *testing.T) {
	c := testClient(t)
	defer c.Close()
	ctx := context.Background()

	if err := c.SetLongURL(ctx, "x", "https://example.com", 0); err == nil {
		t.Errorf("SetLongURL with ttl=0 should error")
	}
	if err := c.SetLongURL(ctx, "x", "https://example.com", -time.Second); err == nil {
		t.Errorf("SetLongURL with ttl<0 should error")
	}
}

func TestCache_DeleteAbsentKeyIsNoError(t *testing.T) {
	c := testClient(t)
	defer c.Close()
	ctx := context.Background()

	if err := c.DeleteLongURL(ctx, uniqueCode("absent")); err != nil {
		t.Errorf("DeleteLongURL of absent key: %v", err)
	}
}

func TestStream_PublishAndLen(t *testing.T) {
	c := testClient(t)
	defer c.Close()
	ctx := context.Background()

	// Baseline length — other tests may have added entries; we measure the delta.
	before, err := c.StreamLen(ctx)
	if err != nil {
		t.Fatalf("StreamLen: %v", err)
	}

	code := uniqueCode("pub")
	for i := 0; i < 5; i++ {
		err := c.PublishClick(ctx, myredis.ClickEvent{
			ShortCode: code,
			Referrer:  "https://news.example.com",
			UserAgent: "Mozilla/5.0 test",
		})
		if err != nil {
			t.Fatalf("PublishClick %d: %v", i, err)
		}
	}

	after, err := c.StreamLen(ctx)
	if err != nil {
		t.Fatalf("StreamLen: %v", err)
	}
	if after-before < 5 {
		t.Errorf("stream grew by %d, want >= 5", after-before)
	}
}

func TestStream_PublishRejectsEmptyCode(t *testing.T) {
	c := testClient(t)
	defer c.Close()
	ctx := context.Background()

	if err := c.PublishClick(ctx, myredis.ClickEvent{}); err == nil {
		t.Errorf("PublishClick with empty code should error")
	}
}

// Verify MAXLEN ~ prevents unbounded stream growth. The `~` means approximate:
// Redis trims at internal-node boundaries (default node size 100), so the
// stream may hold meaningfully more than maxLen at small values. The
// assertion is loose on purpose — what we care about is "writes >> length",
// not a precise cap. Using exact MAXLEN would be more predictable but slower
// per XADD, which defeats the point on the hot path.
func TestStream_MaxLenBoundsGrowth(t *testing.T) {
	c := testClient(t)
	defer c.Close()
	ctx := context.Background()

	const maxLen = 50
	const writes = 500

	if err := rawDel(ctx, c, myredis.StreamName); err != nil {
		t.Fatalf("reset stream: %v", err)
	}

	for i := 0; i < writes; i++ {
		err := c.PublishClickWithMaxLen(ctx, myredis.ClickEvent{
			ShortCode: "maxlen",
		}, maxLen)
		if err != nil {
			t.Fatalf("PublishClickWithMaxLen %d: %v", i, err)
		}
	}

	got, err := c.StreamLen(ctx)
	if err != nil {
		t.Fatalf("StreamLen: %v", err)
	}
	// Must be significantly smaller than total writes (trimming happened) and
	// must be positive (stream still has content).
	if got >= writes/2 {
		t.Errorf("stream length = %d after %d writes with maxLen=%d, expected trimming to keep it well below writes/2", got, writes, maxLen)
	}
	if got < 1 {
		t.Errorf("stream length = %d, want >= 1", got)
	}
}

// rawDel reaches past the package API to DEL a key for test cleanup.
// Kept minimal: we don't want a general "escape hatch" in the public API.
func rawDel(ctx context.Context, c *myredis.Client, key string) error {
	// Use a fresh client pointed at the same addr so we don't need to expose
	// the internal rdb. The indirection is ugly but limited to tests.
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		addr = defaultAddr
	}
	rdb := goredis.NewClient(&goredis.Options{Addr: addr})
	defer rdb.Close()
	return rdb.Del(ctx, key).Err()
}
