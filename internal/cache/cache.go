// Package cache wraps the Redis client used by vidra-search for the short-lived
// suggestion cache (and, in later waves, session recency and trending ZSETs).
// Redis is never the durable source of truth.
package cache

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Cache wraps a Redis client.
type Cache struct {
	Client *redis.Client
}

// New parses a Redis URL, builds a client, and verifies connectivity with a
// ping bounded by ctx.
func New(ctx context.Context, redisURL string) (*Cache, error) {
	opt, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("cache: parse redis url: %w", err)
	}
	client := redis.NewClient(opt)

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("cache: ping: %w", err)
	}
	return &Cache{Client: client}, nil
}

// Get returns the cached bytes for key, or (nil, false) on a miss. Any Redis
// error is treated as a miss so the cache is always best-effort.
func (c *Cache) Get(ctx context.Context, key string) ([]byte, bool) {
	b, err := c.Client.Get(ctx, key).Bytes()
	if err != nil {
		return nil, false
	}
	return b, true
}

// Set stores value under key with the given TTL, best-effort (errors ignored).
func (c *Cache) Set(ctx context.Context, key string, value []byte, ttl time.Duration) {
	_ = c.Client.Set(ctx, key, value, ttl).Err()
}

// Ping checks Redis connectivity, bounded by ctx. Used by readiness probes.
func (c *Cache) Ping(ctx context.Context) error {
	pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return c.Client.Ping(pingCtx).Err()
}

// Close closes the Redis client.
func (c *Cache) Close() error {
	if c.Client != nil {
		return c.Client.Close()
	}
	return nil
}
