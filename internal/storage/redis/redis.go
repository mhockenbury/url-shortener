// Package redis holds the Redis-backed adapters for the two roles Redis plays:
// a read-through cache for short_code → long_url, and a bounded event stream
// used to emit click events from the redirect hot path.
package redis

import (
	"errors"

	"github.com/redis/go-redis/v9"
)

// ErrCacheMiss is returned when a key is not present. Distinct from a
// lower-level Redis error so callers can map it to a storage fallback
// without importing go-redis types.
var ErrCacheMiss = errors.New("cache miss")

// Client bundles a go-redis client and the config used by this package.
// Split into sub-helpers (Cache, Stream) that share the same connection.
type Client struct {
	rdb *redis.Client
}

// NewClient wraps a go-redis client. Caller is responsible for its lifecycle.
func NewClient(rdb *redis.Client) *Client {
	return &Client{rdb: rdb}
}

// Close closes the underlying go-redis client.
func (c *Client) Close() error {
	return c.rdb.Close()
}
