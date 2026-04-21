package redis

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// cacheKey prefixes the short_code so the cache namespace is explicit and
// we can reuse the same Redis for other roles later without collision.
func cacheKey(shortCode string) string {
	return "url:" + shortCode
}

// GetLongURL returns the cached long_url for shortCode. If the key is
// absent, it returns ("", ErrCacheMiss, nil-style) — the caller distinguishes
// miss from error using errors.Is(err, ErrCacheMiss).
func (c *Client) GetLongURL(ctx context.Context, shortCode string) (string, error) {
	v, err := c.rdb.Get(ctx, cacheKey(shortCode)).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return "", ErrCacheMiss
		}
		return "", fmt.Errorf("redis GET %s: %w", cacheKey(shortCode), err)
	}
	return v, nil
}

// SetLongURL writes long_url under shortCode with the given TTL. TTL must
// be > 0; callers should not use this to create keys that never expire.
func (c *Client) SetLongURL(ctx context.Context, shortCode, longURL string, ttl time.Duration) error {
	if ttl <= 0 {
		return errors.New("cache: ttl must be > 0")
	}
	if err := c.rdb.Set(ctx, cacheKey(shortCode), longURL, ttl).Err(); err != nil {
		return fmt.Errorf("redis SET %s: %w", cacheKey(shortCode), err)
	}
	return nil
}

// DeleteLongURL removes the cached entry. Used when a link is deleted or
// its target changes. Absent keys are not an error.
func (c *Client) DeleteLongURL(ctx context.Context, shortCode string) error {
	if err := c.rdb.Del(ctx, cacheKey(shortCode)).Err(); err != nil {
		return fmt.Errorf("redis DEL %s: %w", cacheKey(shortCode), err)
	}
	return nil
}
