package redis

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// StreamName is the Redis Stream receiving click events.
const StreamName = "clicks"

// DefaultStreamMaxLen bounds stream memory when no explicit bound is supplied.
// At 1000 events/s this holds roughly the last ~16 minutes of clicks — enough
// time for the worker to recover from a short outage without dropping events.
const DefaultStreamMaxLen = 1_000_000

// ClickEvent is the payload written to the clicks stream. Kept tiny on
// purpose; the redirect hot path should not pay for serialization of
// dimensions the worker could enrich from the DB or request context.
type ClickEvent struct {
	ShortCode string
	Timestamp time.Time
	Referrer  string
	UserAgent string
}

// PublishClick emits a click event to the stream with an approximate MAXLEN
// cap (MAXLEN ~ N) so Redis can trim efficiently without strict precision.
//
// XADD with MAXLEN ~ is a best-effort write: we don't retry on failure and
// we don't block the redirect path. Callers SHOULD log but not propagate.
func (c *Client) PublishClick(ctx context.Context, ev ClickEvent) error {
	return c.PublishClickWithMaxLen(ctx, ev, DefaultStreamMaxLen)
}

// PublishClickWithMaxLen is PublishClick with an explicit MAXLEN bound.
// Exposed for tests that need to exercise trimming behavior.
func (c *Client) PublishClickWithMaxLen(ctx context.Context, ev ClickEvent, maxLen int64) error {
	if ev.ShortCode == "" {
		return fmt.Errorf("publish click: shortCode is empty")
	}
	ts := ev.Timestamp
	if ts.IsZero() {
		ts = time.Now().UTC()
	}

	args := &redis.XAddArgs{
		Stream: StreamName,
		MaxLen: maxLen,
		Approx: true,
		Values: map[string]any{
			"code": ev.ShortCode,
			"ts":   ts.UTC().UnixMilli(),
			"ref":  ev.Referrer,
			"ua":   ev.UserAgent,
		},
	}
	if err := c.rdb.XAdd(ctx, args).Err(); err != nil {
		return fmt.Errorf("redis XADD %s: %w", StreamName, err)
	}
	return nil
}

// StreamLen returns the current stream length. Mainly for tests and metrics.
func (c *Client) StreamLen(ctx context.Context) (int64, error) {
	n, err := c.rdb.XLen(ctx, StreamName).Result()
	if err != nil {
		return 0, fmt.Errorf("redis XLEN %s: %w", StreamName, err)
	}
	return n, nil
}
