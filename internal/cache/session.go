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

// DropSessionRecency deletes the recent-query and recent-video lists for the
// given sessions. It is the Redis half of a history deletion: those lists are
// read straight back into the account's own autosuggest and session-intent
// ranking, so a clear that only removed rows would keep offering the user the
// queries they just cleared for up to sessionTTL.
//
// Sessions are named explicitly (the caller reads them out of the ledger before
// deleting it) rather than found by SCAN: the keys are keyed by session, not by
// account, so there is no pattern that means "this user's" — and a MATCH sweep
// would walk the whole keyspace on every clear. Best-effort, like every other
// write here; the lists expire on their own within sessionTTL.
func (c *Cache) DropSessionRecency(ctx context.Context, sessionIDs []string) error {
	keys := make([]string, 0, 2*len(sessionIDs))
	for _, id := range sessionIDs {
		if id == "" {
			continue
		}
		keys = append(keys, sessionQueryKey(id), sessionVideoKey(id))
	}
	if len(keys) == 0 {
		return nil
	}
	return c.Client.Del(ctx, keys...).Err()
}
