package cache

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	// sessionTTL bounds how long a session's recent-activity lists live (§1.3).
	sessionTTL = 2 * time.Hour
	// sessionMax caps how many recent items are retained per session.
	sessionMax = 20
)

func sessionQueryKey(sessionID string) string { return "sess:q:" + sessionID }
func sessionVideoKey(sessionID string) string { return "sess:v:" + sessionID }

// pushSession LPUSHes value, trims to the most recent sessionMax, and refreshes
// the TTL — all in one pipeline. Best-effort: an empty session id is a no-op.
func (c *Cache) pushSession(ctx context.Context, key, value string) error {
	if value == "" {
		return nil
	}
	pipe := c.Client.Pipeline()
	pipe.LPush(ctx, key, value)
	pipe.LTrim(ctx, key, 0, sessionMax-1)
	pipe.Expire(ctx, key, sessionTTL)
	_, err := pipe.Exec(ctx)
	return err
}

// PushSessionQuery records a normalized query in the session's recent-queries
// list (used by the suggestion session stream).
func (c *Cache) PushSessionQuery(ctx context.Context, sessionID, normalizedQuery string) error {
	if sessionID == "" {
		return nil
	}
	return c.pushSession(ctx, sessionQueryKey(sessionID), normalizedQuery)
}

// PushSessionVideo records a video id in the session's recent-videos list.
func (c *Cache) PushSessionVideo(ctx context.Context, sessionID, videoID string) error {
	if sessionID == "" {
		return nil
	}
	return c.pushSession(ctx, sessionVideoKey(sessionID), videoID)
}

// SessionQueries returns the session's recent normalized queries, newest first.
// A Redis error yields an empty slice so callers can treat it as best-effort.
func (c *Cache) SessionQueries(ctx context.Context, sessionID string) []string {
	if sessionID == "" {
		return nil
	}
	vals, err := c.Client.LRange(ctx, sessionQueryKey(sessionID), 0, sessionMax-1).Result()
	if err != nil && err != redis.Nil {
		return nil
	}
	return vals
}

// SessionVideos returns the session's recent video ids (sess:v), newest first —
// the session-intent signal for advanced ranking and the session-based candidate
// seed for advanced recommendations. Best-effort: a Redis error yields nil.
func (c *Cache) SessionVideos(ctx context.Context, sessionID string) []string {
	if sessionID == "" {
		return nil
	}
	vals, err := c.Client.LRange(ctx, sessionVideoKey(sessionID), 0, sessionMax-1).Result()
	if err != nil && err != redis.Nil {
		return nil
	}
	return vals
}
