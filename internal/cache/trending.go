package cache

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/vidra/vidra-search/internal/trending"
)

// counterTTL bounds the per-day distinct-user (HLL) and raw-count keys. 8 days
// covers a multi-day trending window plus slack (§1.3).
const counterTTL = 8 * 24 * time.Hour

func trendZKey(domain string) string         { return "trend:" + domain }
func trendTSKey(domain string) string        { return "trend:" + domain + ":ts" }
func trendTopKey(domain string) string       { return "trend:" + domain + ":top" }
func hllKey(domain, item, day string) string { return "hll:" + domain + ":" + item + ":" + day }
func cntKey(domain, item, day string) string { return "cnt:" + domain + ":" + item + ":" + day }
func trendGuardKey(domain, subject, item string) string {
	return "guard:trend:" + domain + ":" + subject + ":" + item
}

func dayStamp(t time.Time) string { return t.UTC().Format("20060102") }

// trendBumpScript atomically decays a member's stored score to now, adds inc, and
// refreshes the last-update timestamp + key TTLs. Keeping decay-then-increment in
// one script makes concurrent event ingestion race-free (§1.9).
var trendBumpScript = redis.NewScript(`
local cur = tonumber(redis.call('ZSCORE', KEYS[1], ARGV[1]) or '0')
local last = tonumber(redis.call('HGET', KEYS[2], ARGV[1]) or ARGV[2])
local now = tonumber(ARGV[2])
local hl = tonumber(ARGV[3])
local inc = tonumber(ARGV[4])
local ttl = tonumber(ARGV[5])
local elapsed = now - last
local factor = 1.0
if hl > 0 and elapsed > 0 then factor = 2 ^ (-elapsed / hl) end
local newv = cur * factor + inc
redis.call('ZADD', KEYS[1], newv, ARGV[1])
redis.call('HSET', KEYS[2], ARGV[1], now)
redis.call('EXPIRE', KEYS[1], ttl)
redis.call('EXPIRE', KEYS[2], ttl)
return tostring(newv)
`)

// trendSweepScript decays every member of a trend ZSET to now and prunes those
// that have fallen below a floor, in one atomic pass. Because it is atomic, any
// bump queued during the sweep applies cleanly before or after it (no lost
// increments). Returns the surviving member count.
var trendSweepScript = redis.NewScript(`
local now = tonumber(ARGV[1])
local hl = tonumber(ARGV[2])
local floor = tonumber(ARGV[3])
local members = redis.call('ZRANGE', KEYS[1], 0, -1, 'WITHSCORES')
for i = 1, #members, 2 do
  local m = members[i]
  local s = tonumber(members[i+1])
  local last = tonumber(redis.call('HGET', KEYS[2], m) or now)
  local elapsed = now - last
  local factor = 1.0
  if hl > 0 and elapsed > 0 then factor = 2 ^ (-elapsed / hl) end
  local decayed = s * factor
  if decayed < floor then
    redis.call('ZREM', KEYS[1], m)
    redis.call('HDEL', KEYS[2], m)
  else
    redis.call('ZADD', KEYS[1], decayed, m)
    redis.call('HSET', KEYS[2], m, now)
  end
end
return redis.call('ZCARD', KEYS[1])
`)

// TrendBump records one contribution toward an item's trending score in the given
// domain ("q" for queries, "v" for videos). It always bumps the uncapped per-day
// total counter and the distinct-user HLL (subject deduped naturally), but the
// decayed ranking ZSET is bumped at most once per (subject, item) per capWindow —
// so a single user cannot inflate the ranking, while their single distinct-user
// contribution is still counted. Best-effort; an empty subject folds to "anon".
func (c *Cache) TrendBump(ctx context.Context, domain, item, subject string, halfLifeSeconds float64, capWindow time.Duration) error {
	if item == "" {
		return nil
	}
	if subject == "" {
		subject = "anon"
	}
	day := dayStamp(time.Now())

	pipe := c.Client.Pipeline()
	ck := cntKey(domain, item, day)
	pipe.Incr(ctx, ck)
	pipe.Expire(ctx, ck, counterTTL)
	hk := hllKey(domain, item, day)
	pipe.PFAdd(ctx, hk, subject)
	pipe.Expire(ctx, hk, counterTTL)
	if _, err := pipe.Exec(ctx); err != nil {
		return err
	}

	set, err := c.Client.SetNX(ctx, trendGuardKey(domain, subject, item), 1, capWindow).Result()
	if err != nil {
		return err
	}
	if !set {
		return nil // this (subject,item) already counted toward ranking this window
	}
	now := strconv.FormatInt(time.Now().Unix(), 10)
	return trendBumpScript.Run(ctx, c.Client,
		[]string{trendZKey(domain), trendTSKey(domain)},
		item, now, strconv.FormatFloat(halfLifeSeconds, 'f', -1, 64), "1",
		strconv.FormatInt(int64(counterTTL.Seconds()), 10),
	).Err()
}

// TrendSweep decays and prunes a domain's trend ZSET, returning the surviving
// member count.
func (c *Cache) TrendSweep(ctx context.Context, domain string, halfLifeSeconds, floor float64) (int64, error) {
	now := strconv.FormatInt(time.Now().Unix(), 10)
	return trendSweepScript.Run(ctx, c.Client,
		[]string{trendZKey(domain), trendTSKey(domain)},
		now, strconv.FormatFloat(halfLifeSeconds, 'f', -1, 64), strconv.FormatFloat(floor, 'f', -1, 64),
	).Int64()
}

// TrendTop returns the top-k members of a domain's trend ZSET by (decayed) score.
func (c *Cache) TrendTop(ctx context.Context, domain string, k int) ([]trending.Scored, error) {
	if k <= 0 {
		return nil, nil
	}
	zs, err := c.Client.ZRevRangeWithScores(ctx, trendZKey(domain), 0, int64(k-1)).Result()
	if err != nil {
		return nil, err
	}
	out := make([]trending.Scored, 0, len(zs))
	for _, z := range zs {
		if m, ok := z.Member.(string); ok {
			out = append(out, trending.Scored{Item: m, Score: z.Score})
		}
	}
	return out, nil
}

// TrendDistinctUsers returns the distinct-subject estimate (merged HLL) for an
// item over the trailing `days` days.
func (c *Cache) TrendDistinctUsers(ctx context.Context, domain, item string, days int) (int64, error) {
	keys := dayKeys(hllKey, domain, item, days)
	return c.Client.PFCount(ctx, keys...).Result()
}

// TrendTotal returns the total (uncapped) contribution count for an item over the
// trailing `days` days.
func (c *Cache) TrendTotal(ctx context.Context, domain, item string, days int) (float64, error) {
	keys := dayKeys(cntKey, domain, item, days)
	vals, err := c.Client.MGet(ctx, keys...).Result()
	if err != nil {
		return 0, err
	}
	var total float64
	for _, v := range vals {
		if s, ok := v.(string); ok {
			if n, err := strconv.ParseFloat(s, 64); err == nil {
				total += n
			}
		}
	}
	return total, nil
}

// dayKeys builds the per-day keys for the trailing `days` days (today first).
func dayKeys(keyFn func(domain, item, day string) string, domain, item string, days int) []string {
	if days < 1 {
		days = 1
	}
	now := time.Now().UTC()
	keys := make([]string, 0, days)
	for i := 0; i < days; i++ {
		keys = append(keys, keyFn(domain, item, dayStamp(now.AddDate(0, 0, -i))))
	}
	return keys
}

// WriteTrendingList caches a gated trending list as JSON under trend:{domain}:top.
func (c *Cache) WriteTrendingList(ctx context.Context, domain string, items []trending.Scored, ttl time.Duration) error {
	raw, err := json.Marshal(items)
	if err != nil {
		return err
	}
	return c.Client.Set(ctx, trendTopKey(domain), raw, ttl).Err()
}

// TrendingQuerySet returns the cached trending queries as item→score, or an empty
// map on any miss/error (best-effort; used to boost suggestions).
func (c *Cache) TrendingQuerySet(ctx context.Context) map[string]float64 {
	out := map[string]float64{}
	for _, s := range c.readTrendingList(ctx, "q") {
		out[s.Item] = s.Score
	}
	return out
}

// TrendingVideos returns the cached gated trending videos in order, or nil on any
// miss/error (home recommendations fall back to SQL gravity then).
func (c *Cache) TrendingVideos(ctx context.Context) []trending.Scored {
	return c.readTrendingList(ctx, "v")
}

func (c *Cache) readTrendingList(ctx context.Context, domain string) []trending.Scored {
	raw, err := c.Client.Get(ctx, trendTopKey(domain)).Bytes()
	if err != nil {
		return nil
	}
	var items []trending.Scored
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil
	}
	return items
}
