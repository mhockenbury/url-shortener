// Package events wraps Redis-Stream consumer-group mechanics used by the
// analytics worker. The producer side lives in internal/storage/redis.
//
// Delivery model: at-least-once. Duplicate events are accepted as small
// analytics overcount (see README §6 tradeoffs). The consumer reads a batch,
// the worker inserts it into ClickHouse, then the consumer acks.
package events

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// DefaultStream and DefaultGroup match what the producer uses.
const (
	DefaultStream = "clicks"
	DefaultGroup  = "analytics"
)

// ClickEvent is the decoded shape of a stream entry. Matches the fields the
// producer writes in internal/storage/redis/stream.go.
type ClickEvent struct {
	ID        string // Redis stream entry ID, used for Ack
	ShortCode string
	Timestamp time.Time
	Referrer  string
	UserAgent string
}

// Consumer is a thin wrapper over go-redis XREADGROUP / XACK / XPENDING /
// XCLAIM aimed at a single stream + group + consumer triple.
type Consumer struct {
	rdb      *redis.Client
	stream   string
	group    string
	consumer string
}

// NewConsumer constructs a consumer. `consumer` should be unique per worker
// instance (e.g., hostname+pid) so XPENDING / XCLAIM can distinguish who
// owns an unacked entry.
func NewConsumer(rdb *redis.Client, stream, group, consumer string) *Consumer {
	return &Consumer{
		rdb:      rdb,
		stream:   stream,
		group:    group,
		consumer: consumer,
	}
}

// EnsureGroup creates the consumer group if it doesn't exist. Idempotent:
// the BUSYGROUP error returned by Redis when the group already exists is
// swallowed. `MKSTREAM` creates the stream if it doesn't yet have any entries
// so worker startup doesn't race with the first producer write.
func (c *Consumer) EnsureGroup(ctx context.Context) error {
	err := c.rdb.XGroupCreateMkStream(ctx, c.stream, c.group, "$").Err()
	if err != nil {
		// Redis returns a BUSYGROUP error — exact string match is the
		// documented way to detect it.
		if err.Error() == "BUSYGROUP Consumer Group name already exists" {
			return nil
		}
		return fmt.Errorf("xgroup create %s/%s: %w", c.stream, c.group, err)
	}
	return nil
}

// Read blocks for up to `block` waiting for new entries, returning up to
// `count` decoded events. Returns an empty slice (not an error) on timeout
// so the worker loop can check context cancellation between reads.
func (c *Consumer) Read(ctx context.Context, count int64, block time.Duration) ([]ClickEvent, error) {
	if count <= 0 {
		return nil, errors.New("events: count must be > 0")
	}

	res, err := c.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    c.group,
		Consumer: c.consumer,
		Streams:  []string{c.stream, ">"}, // ">" = only new (not-yet-delivered) entries
		Count:    count,
		Block:    block,
	}).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			// Timeout with no entries — not an error from our perspective.
			return nil, nil
		}
		return nil, fmt.Errorf("xreadgroup: %w", err)
	}
	return decodeXStreams(res)
}

// Ack acknowledges successfully-processed entries so they stop appearing in
// XPENDING. Must be called after the downstream write (ClickHouse insert)
// commits, or delivery is effectively best-effort rather than at-least-once.
func (c *Consumer) Ack(ctx context.Context, ids ...string) error {
	if len(ids) == 0 {
		return nil
	}
	if err := c.rdb.XAck(ctx, c.stream, c.group, ids...).Err(); err != nil {
		return fmt.Errorf("xack: %w", err)
	}
	return nil
}

// ReclaimPending takes ownership of entries that another consumer has held
// without acking for at least `idleThreshold`. Returns them as fresh events
// the caller can retry. Used on worker startup to pick up work left behind
// by a crashed predecessor.
//
// Uses XAUTOCLAIM which is both simpler and more efficient than the
// XPENDING + XCLAIM pair for this use case.
func (c *Consumer) ReclaimPending(ctx context.Context, idleThreshold time.Duration, count int64) ([]ClickEvent, error) {
	if count <= 0 {
		return nil, errors.New("events: count must be > 0")
	}

	msgs, _, err := c.rdb.XAutoClaim(ctx, &redis.XAutoClaimArgs{
		Stream:   c.stream,
		Group:    c.group,
		Consumer: c.consumer,
		MinIdle:  idleThreshold,
		Start:    "0-0",
		Count:    count,
	}).Result()
	if err != nil {
		return nil, fmt.Errorf("xautoclaim: %w", err)
	}
	// XAutoClaim returns decoded messages directly (no wrapping XStream layer).
	return decodeXMessages(msgs)
}

// decodeXStreams flattens the XReadGroup response (one XStream per stream
// even though we only read one stream) into a slice of ClickEvent.
func decodeXStreams(streams []redis.XStream) ([]ClickEvent, error) {
	var total int
	for _, s := range streams {
		total += len(s.Messages)
	}
	out := make([]ClickEvent, 0, total)
	for _, s := range streams {
		evs, err := decodeXMessages(s.Messages)
		if err != nil {
			return nil, err
		}
		out = append(out, evs...)
	}
	return out, nil
}

func decodeXMessages(msgs []redis.XMessage) ([]ClickEvent, error) {
	out := make([]ClickEvent, 0, len(msgs))
	for _, m := range msgs {
		ev, err := decodeOne(m)
		if err != nil {
			return nil, fmt.Errorf("decode entry %s: %w", m.ID, err)
		}
		out = append(out, ev)
	}
	return out, nil
}

// decodeOne turns one stream entry's Values map into a ClickEvent. Missing
// fields are treated as empty strings (matches producer behavior for
// referrer/user-agent when absent); a missing `code` surfaces as an error.
func decodeOne(m redis.XMessage) (ClickEvent, error) {
	ev := ClickEvent{ID: m.ID}

	code, ok := m.Values["code"].(string)
	if !ok || code == "" {
		return ClickEvent{}, errors.New("missing or empty 'code'")
	}
	ev.ShortCode = code

	if v, ok := m.Values["ts"].(string); ok && v != "" {
		ms, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return ClickEvent{}, fmt.Errorf("parse ts %q: %w", v, err)
		}
		ev.Timestamp = time.UnixMilli(ms).UTC()
	}
	if v, ok := m.Values["ref"].(string); ok {
		ev.Referrer = v
	}
	if v, ok := m.Values["ua"].(string); ok {
		ev.UserAgent = v
	}
	return ev, nil
}
