package events_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/mhockenbury/url-shortener/internal/events"
	myredis "github.com/mhockenbury/url-shortener/internal/storage/redis"
)

// Integration tests use a fresh stream+group per test so runs don't
// interfere with each other, and skip if Redis isn't reachable.
//
// Run locally:
//   docker compose up -d redis
//   REDIS_ADDR=localhost:6379 go test ./internal/events/

const defaultAddr = "localhost:6379"

func testRedis(t *testing.T) *goredis.Client {
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
	return rdb
}

// freshStream returns (streamName, groupName) unique to this test, and
// registers cleanup that deletes the stream afterward.
func freshStream(t *testing.T, rdb *goredis.Client) (string, string) {
	t.Helper()
	suffix := fmt.Sprintf("%s_%d", t.Name(), time.Now().UnixNano())
	stream := "test_stream_" + suffix
	group := "test_group_" + suffix
	t.Cleanup(func() {
		_ = rdb.Del(context.Background(), stream).Err()
	})
	return stream, group
}

// publish writes one event directly via go-redis so tests don't depend on the
// producer package's exact XADD shape beyond the field names the consumer decodes.
func publish(t *testing.T, rdb *goredis.Client, stream, code string, ts time.Time) string {
	t.Helper()
	id, err := rdb.XAdd(context.Background(), &goredis.XAddArgs{
		Stream: stream,
		Values: map[string]any{
			"code": code,
			"ts":   ts.UTC().UnixMilli(),
			"ref":  "https://ref.example.com",
			"ua":   "test-agent",
		},
	}).Result()
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	return id
}

func TestConsumer_EnsureGroupIsIdempotent(t *testing.T) {
	rdb := testRedis(t)
	defer rdb.Close()
	stream, group := freshStream(t, rdb)

	c := events.NewConsumer(rdb, stream, group, "c1")
	if err := c.EnsureGroup(context.Background()); err != nil {
		t.Fatalf("first EnsureGroup: %v", err)
	}
	if err := c.EnsureGroup(context.Background()); err != nil {
		t.Fatalf("second EnsureGroup: %v", err)
	}
}

func TestConsumer_ReadDecodesEvents(t *testing.T) {
	rdb := testRedis(t)
	defer rdb.Close()
	stream, group := freshStream(t, rdb)

	c := events.NewConsumer(rdb, stream, group, "c1")
	if err := c.EnsureGroup(context.Background()); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}

	ts := time.Now().UTC().Truncate(time.Millisecond)
	id := publish(t, rdb, stream, "abc123", ts)

	got, err := c.Read(context.Background(), 10, 200*time.Millisecond)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1", len(got))
	}
	ev := got[0]
	if ev.ID != id {
		t.Errorf("ID = %q, want %q", ev.ID, id)
	}
	if ev.ShortCode != "abc123" {
		t.Errorf("ShortCode = %q", ev.ShortCode)
	}
	if !ev.Timestamp.Equal(ts) {
		t.Errorf("Timestamp = %v, want %v", ev.Timestamp, ts)
	}
	if ev.Referrer != "https://ref.example.com" {
		t.Errorf("Referrer = %q", ev.Referrer)
	}
	if ev.UserAgent != "test-agent" {
		t.Errorf("UserAgent = %q", ev.UserAgent)
	}
}

func TestConsumer_ReadReturnsEmptyOnTimeout(t *testing.T) {
	rdb := testRedis(t)
	defer rdb.Close()
	stream, group := freshStream(t, rdb)

	c := events.NewConsumer(rdb, stream, group, "c1")
	if err := c.EnsureGroup(context.Background()); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}

	got, err := c.Read(context.Background(), 10, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d events, want 0 on timeout", len(got))
	}
}

func TestConsumer_AckRemovesFromPending(t *testing.T) {
	rdb := testRedis(t)
	defer rdb.Close()
	stream, group := freshStream(t, rdb)

	c := events.NewConsumer(rdb, stream, group, "c1")
	if err := c.EnsureGroup(context.Background()); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}

	publish(t, rdb, stream, "ackme", time.Now())
	got, err := c.Read(context.Background(), 10, 200*time.Millisecond)
	if err != nil || len(got) != 1 {
		t.Fatalf("Read: got %d events, err=%v", len(got), err)
	}

	// Before ack: XPENDING should report one entry.
	pend, err := rdb.XPending(context.Background(), stream, group).Result()
	if err != nil {
		t.Fatalf("XPending: %v", err)
	}
	if pend.Count != 1 {
		t.Fatalf("pending before ack = %d, want 1", pend.Count)
	}

	if err := c.Ack(context.Background(), got[0].ID); err != nil {
		t.Fatalf("Ack: %v", err)
	}

	pend, err = rdb.XPending(context.Background(), stream, group).Result()
	if err != nil {
		t.Fatalf("XPending: %v", err)
	}
	if pend.Count != 0 {
		t.Fatalf("pending after ack = %d, want 0", pend.Count)
	}
}

func TestConsumer_AckEmptyIsNoOp(t *testing.T) {
	rdb := testRedis(t)
	defer rdb.Close()
	stream, group := freshStream(t, rdb)

	c := events.NewConsumer(rdb, stream, group, "c1")
	if err := c.EnsureGroup(context.Background()); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}

	if err := c.Ack(context.Background()); err != nil {
		t.Fatalf("Ack with no ids: %v", err)
	}
}

// Simulate a worker that reads but crashes before ack; a fresh worker
// should reclaim the pending entry after the idle threshold.
func TestConsumer_ReclaimPendingHandsOffAbandonedEntries(t *testing.T) {
	rdb := testRedis(t)
	defer rdb.Close()
	stream, group := freshStream(t, rdb)

	worker1 := events.NewConsumer(rdb, stream, group, "worker-1")
	worker2 := events.NewConsumer(rdb, stream, group, "worker-2")
	if err := worker1.EnsureGroup(context.Background()); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}

	publish(t, rdb, stream, "abandoned", time.Now())

	got, err := worker1.Read(context.Background(), 10, 200*time.Millisecond)
	if err != nil || len(got) != 1 {
		t.Fatalf("worker1.Read: got %d err=%v", len(got), err)
	}
	// Do NOT ack — worker1 "crashes". Wait past the idle threshold.
	time.Sleep(150 * time.Millisecond)

	reclaimed, err := worker2.ReclaimPending(context.Background(), 100*time.Millisecond, 10)
	if err != nil {
		t.Fatalf("ReclaimPending: %v", err)
	}
	if len(reclaimed) != 1 {
		t.Fatalf("reclaimed %d, want 1", len(reclaimed))
	}
	if reclaimed[0].ShortCode != "abandoned" {
		t.Errorf("ShortCode = %q, want %q", reclaimed[0].ShortCode, "abandoned")
	}
	if reclaimed[0].ID != got[0].ID {
		t.Errorf("reclaimed ID = %q, original = %q", reclaimed[0].ID, got[0].ID)
	}
}

// Producer <-> consumer contract check: events written via the real
// producer decode correctly on the consumer side. Catches drift between
// the two packages.
func TestConsumer_DecodesProducerOutput(t *testing.T) {
	rdb := testRedis(t)
	defer rdb.Close()

	// The producer writes to the "clicks" stream; we read from it via a
	// distinct group unique to this test so we don't fight the real worker
	// (which doesn't exist yet but eventually will).
	group := fmt.Sprintf("test_group_producer_%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_ = rdb.XGroupDestroy(context.Background(), myredis.StreamName, group).Err()
	})

	c := events.NewConsumer(rdb, myredis.StreamName, group, "c1")
	if err := c.EnsureGroup(context.Background()); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}

	prod := myredis.NewClient(rdb)
	code := fmt.Sprintf("pc%d", time.Now().UnixNano())
	err := prod.PublishClick(context.Background(), myredis.ClickEvent{
		ShortCode: code,
		Referrer:  "https://news.example.com",
		UserAgent: "mozilla/decode-test",
	})
	if err != nil {
		t.Fatalf("PublishClick: %v", err)
	}

	// Read may return other events from the shared stream; filter for ours.
	deadline := time.Now().Add(2 * time.Second)
	var found *events.ClickEvent
	for time.Now().Before(deadline) && found == nil {
		got, err := c.Read(context.Background(), 50, 200*time.Millisecond)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		for i := range got {
			if got[i].ShortCode == code {
				found = &got[i]
				break
			}
		}
	}
	if found == nil {
		t.Fatal("producer-published event never arrived at consumer")
	}
	if found.Referrer != "https://news.example.com" {
		t.Errorf("Referrer = %q", found.Referrer)
	}
	if found.UserAgent != "mozilla/decode-test" {
		t.Errorf("UserAgent = %q", found.UserAgent)
	}
	if found.Timestamp.IsZero() {
		t.Errorf("Timestamp is zero, producer should set it")
	}
}
