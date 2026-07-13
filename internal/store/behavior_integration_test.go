//go:build integration

// W2 integration tests: behavioral event persistence, the rollup/sessionizer/
// trending/retention workers, the history + privacy endpoints, and the privacy /
// manipulation-resistance guarantees. They run against the same live Postgres +
// Redis as the W1 suite and self-skip when DATABASE_URL/REDIS_URL are unset.
package store_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vidra/vidra-search/internal/event"
	"github.com/vidra/vidra-search/internal/suggest"
)

// --- behavioral envelope builders ---

func behEnv(typ string, occurredAt time.Time, payload map[string]any) event.Envelope {
	raw, _ := json.Marshal(payload)
	return event.Envelope{EventID: uuid.New(), Type: typ, OccurredAt: occurredAt, SchemaVersion: 1, Payload: raw}
}

func submitted(occurredAt time.Time, query string, user *uuid.UUID, session string, allowHistory bool) event.Envelope {
	p := map[string]any{"query": query, "allow_history": allowHistory}
	if user != nil {
		p["user_id"] = user.String()
	}
	if session != "" {
		p["session_id"] = session
	}
	return behEnv(event.TypeSearchSubmitted, occurredAt, p)
}

func play(occurredAt time.Time, videoID uuid.UUID, query string, user *uuid.UUID, session string, allowHistory bool) event.Envelope {
	p := map[string]any{"video_id": videoID.String(), "context": "search", "allow_history": allowHistory}
	if query != "" {
		p["query"] = query
	}
	if user != nil {
		p["user_id"] = user.String()
	}
	if session != "" {
		p["session_id"] = session
	}
	return behEnv(event.TypeVideoPlayStarted, occurredAt, p)
}

func watch(occurredAt time.Time, videoID uuid.UUID, positionSeconds float64, user *uuid.UUID, session string, allowHistory bool) event.Envelope {
	p := map[string]any{"video_id": videoID.String(), "position_seconds": positionSeconds, "allow_history": allowHistory}
	if user != nil {
		p["user_id"] = user.String()
	}
	if session != "" {
		p["session_id"] = session
	}
	return behEnv(event.TypeVideoWatchProgress, occurredAt, p)
}

func clicked(occurredAt time.Time, query string, videoID uuid.UUID, user *uuid.UUID, session string) event.Envelope {
	p := map[string]any{"query": query, "video_id": videoID.String()}
	if user != nil {
		p["user_id"] = user.String()
	}
	if session != "" {
		p["session_id"] = session
	}
	return behEnv(event.TypeSearchResultClicked, occurredAt, p)
}

func impression(occurredAt time.Time, query string, videoID uuid.UUID, session string) event.Envelope {
	return behEnv(event.TypeVideoImpression, occurredAt, map[string]any{
		"query": query, "video_id": videoID.String(), "session_id": session,
	})
}

func countRows(t *testing.T, env *testEnv, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := env.store.Pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	return n
}

func runWorker(t *testing.T, env *testEnv, name string) {
	t.Helper()
	if err := env.worker.RunOnce(context.Background(), name); err != nil {
		t.Fatalf("worker %s: %v", name, err)
	}
}

func containsStr(s []string, want string) bool {
	for _, x := range s {
		if x == want {
			return true
		}
	}
	return false
}

// --- tests ---

// TestIntegrationAggregatesRollupSuggestible proves a query becomes suggestible
// only once it clears the distinct-user threshold, end to end.
func TestIntegrationAggregatesRollupSuggestible(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	now := time.Now()

	var batch []event.Envelope
	for i := 0; i < 3; i++ {
		u := uuid.New()
		batch = append(batch, submitted(now, "kubernetes deployment", &u, "", false))
	}
	lonely := uuid.New()
	batch = append(batch, submitted(now, "obscureterm xyzzy", &lonely, "", false))
	ingest(t, env, batch...)

	runWorker(t, env, "aggregates_rollup")

	// kubernetes: 3 distinct users → suggestible; obscure: 1 → not.
	kubDistinct := countRows(t, env, "SELECT distinct_users FROM search.query_aggregates WHERE normalized_query = 'kubernetes deployment'")
	if kubDistinct != 3 {
		t.Errorf("kubernetes distinct_users = %d, want 3", kubDistinct)
	}
	kubSuggestible := countRows(t, env, "SELECT count(*) FROM search.query_aggregates WHERE normalized_query = 'kubernetes deployment' AND suggestible")
	if kubSuggestible != 1 {
		t.Errorf("kubernetes must be suggestible")
	}
	obsSuggestible := countRows(t, env, "SELECT count(*) FROM search.query_aggregates WHERE normalized_query = 'obscureterm xyzzy' AND suggestible")
	if obsSuggestible != 0 {
		t.Errorf("single-user query must not be suggestible")
	}

	// The aggregate suggestion stream surfaces only the suggestible query.
	resp := env.sugg.Suggest(ctx, suggest.Request{Query: "kub", Limit: 10})
	if !containsStr(suggestionTexts(resp.Suggestions), "kubernetes deployment") {
		t.Errorf("expected 'kubernetes deployment' suggestion, got %v", suggestionTexts(resp.Suggestions))
	}
	respObs := env.sugg.Suggest(ctx, suggest.Request{Query: "obsc", Limit: 10})
	if containsStr(suggestionTexts(respObs.Suggestions), "obscureterm xyzzy") {
		t.Errorf("below-threshold query must not be suggested, got %v", suggestionTexts(respObs.Suggestions))
	}
}

// TestIntegrationManipulationResistance: one user submitting the same query 1000×
// must never become suggestible NOR trending — distinct users stays 1.
func TestIntegrationManipulationResistance(t *testing.T) {
	env := newTestEnv(t)
	now := time.Now()
	spammer := uuid.New()

	// 1000 identical submissions in two ≤500 batches.
	for b := 0; b < 2; b++ {
		var batch []event.Envelope
		for i := 0; i < 500; i++ {
			batch = append(batch, submitted(now, "buy followers now", &spammer, "sess-spam", false))
		}
		ingest(t, env, batch...)
	}

	runWorker(t, env, "aggregates_rollup")
	runWorker(t, env, "trending_sweeper")

	distinct := countRows(t, env, "SELECT distinct_users FROM search.query_aggregates WHERE normalized_query = 'buy followers now'")
	if distinct != 1 {
		t.Errorf("spam query distinct_users = %d, want 1", distinct)
	}
	suggestible := countRows(t, env, "SELECT count(*) FROM search.query_aggregates WHERE normalized_query = 'buy followers now' AND suggestible")
	if suggestible != 0 {
		t.Errorf("spam query must NOT be suggestible")
	}
	total := countRows(t, env, "SELECT total_count FROM search.query_aggregates WHERE normalized_query = 'buy followers now'")
	if total != 1000 {
		t.Errorf("total_count = %d, want 1000 (volume counted, but distinct gated)", total)
	}
	if set := env.cache.TrendingQuerySet(context.Background()); set["buy followers now"] != 0 {
		t.Errorf("spam query must NOT trend, trend set = %v", set)
	}
}

// TestIntegrationTrendingGatePositive: a query from enough distinct users passes
// the gate and is published to the trending list.
func TestIntegrationTrendingGatePositive(t *testing.T) {
	env := newTestEnv(t)
	now := time.Now()

	var batch []event.Envelope
	for i := 0; i < 4; i++ {
		u := uuid.New()
		batch = append(batch, submitted(now, "world cup final", &u, "", false))
	}
	ingest(t, env, batch...)

	runWorker(t, env, "trending_sweeper")

	set := env.cache.TrendingQuerySet(context.Background())
	if _, ok := set["world cup final"]; !ok {
		t.Errorf("a 4-distinct-user query should trend, trend set = %v", set)
	}
}

// TestIntegrationAllowHistoryEnforcement is the CRITICAL privacy guarantee: events
// WITHOUT allow_history never write the durable personal projections, though the
// raw ledgers are still populated.
func TestIntegrationAllowHistoryEnforcement(t *testing.T) {
	env := newTestEnv(t)
	now := time.Now()
	u := uuid.New()
	v := uuid.New()

	ingest(t, env,
		submitted(now, "private matter", &u, "sess-x", false), // NO allow_history
		play(now, v, "private matter", &u, "sess-x", false),   // NO allow_history
		watch(now, v, 90, &u, "sess-x", false),                // NO allow_history
	)

	// Raw ledgers ARE populated.
	if n := countRows(t, env, "SELECT count(*) FROM search.query_log WHERE normalized_query = 'private matter'"); n != 1 {
		t.Errorf("query_log must record the search, got %d", n)
	}
	if n := countRows(t, env, "SELECT count(*) FROM search.behavior_events WHERE user_id = $1", u); n != 3 {
		t.Errorf("behavior_events must record all three events, got %d", n)
	}
	// Durable personal projections are NOT.
	if n := countRows(t, env, "SELECT count(*) FROM search.user_search_history WHERE user_id = $1", u); n != 0 {
		t.Errorf("NO search history may be written without allow_history, got %d", n)
	}
	// Even after the engagement rollup derives a meaningful watch, no projection.
	runWorker(t, env, "engagement_rollup")
	if n := countRows(t, env, "SELECT count(*) FROM search.user_watch_projection WHERE user_id = $1", u); n != 0 {
		t.Errorf("NO watch projection may be written without allow_history, got %d", n)
	}
}

// TestIntegrationHistoryEndpointsAndPurge covers list, per-entry delete, clear
// (with anonymization), and full user purge.
func TestIntegrationHistoryEndpointsAndPurge(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	now := time.Now()
	u := uuid.New()
	v := uuid.New()

	ingest(t, env,
		submitted(now, "docker compose", &u, "s1", true),
		submitted(now.Add(time.Second), "docker compose", &u, "s1", true), // repeat → use_count 2
		submitted(now.Add(2*time.Second), "kubernetes pods", &u, "s1", true),
		play(now, v, "docker compose", &u, "s1", true), // projection
	)

	list, err := env.history.List(ctx, u, 0, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list.Entries) != 2 {
		t.Fatalf("expected 2 history entries, got %d", len(list.Entries))
	}
	var dockerCount int32
	for _, e := range list.Entries {
		if e.NormalizedQuery == "docker compose" {
			dockerCount = e.UseCount
		}
	}
	if dockerCount != 2 {
		t.Errorf("docker compose use_count = %d, want 2", dockerCount)
	}

	// Per-entry delete.
	if err := env.history.DeleteEntry(ctx, u, "kubernetes pods"); err != nil {
		t.Fatalf("delete entry: %v", err)
	}
	if n := countRows(t, env, "SELECT count(*) FROM search.user_search_history WHERE user_id = $1", u); n != 1 {
		t.Errorf("after entry delete expected 1 history row, got %d", n)
	}

	// Clear all: history gone, raw logs anonymized, projection still present.
	if err := env.history.ClearAll(ctx, u); err != nil {
		t.Fatalf("clear all: %v", err)
	}
	if n := countRows(t, env, "SELECT count(*) FROM search.user_search_history WHERE user_id = $1", u); n != 0 {
		t.Errorf("clear all must remove history, got %d rows", n)
	}
	if n := countRows(t, env, "SELECT count(*) FROM search.query_log WHERE user_id = $1", u); n != 0 {
		t.Errorf("clear all must anonymize query_log user_id, got %d rows still referencing user", n)
	}
	if n := countRows(t, env, "SELECT count(*) FROM search.behavior_events WHERE user_id = $1", u); n != 0 {
		t.Errorf("clear all must anonymize behavior_events user_id, got %d rows", n)
	}
	if n := countRows(t, env, "SELECT count(*) FROM search.user_watch_projection WHERE user_id = $1", u); n != 1 {
		t.Errorf("clear all (search scope) must keep the watch projection, got %d", n)
	}

	// Full purge: nothing references the user anywhere.
	if err := env.history.PurgeUser(ctx, u); err != nil {
		t.Fatalf("purge: %v", err)
	}
	for _, tbl := range []string{"user_search_history", "user_watch_projection"} {
		if n := countRows(t, env, "SELECT count(*) FROM search."+tbl+" WHERE user_id = $1", u); n != 0 {
			t.Errorf("purge must empty %s for the user, got %d", tbl, n)
		}
	}
}

// TestIntegrationRetentionDeletes proves the retention worker drops rows older
// than the window while keeping recent ones.
func TestIntegrationRetentionDeletes(t *testing.T) {
	env := newTestEnv(t)
	now := time.Now()
	old := now.AddDate(0, 0, -100) // older than the 90-day default
	u := uuid.New()

	ingest(t, env,
		submitted(old, "ancient query", &u, "", false),
		submitted(now, "fresh query", &u, "", false),
	)

	runWorker(t, env, "retention")

	if n := countRows(t, env, "SELECT count(*) FROM search.query_log WHERE normalized_query = 'ancient query'"); n != 0 {
		t.Errorf("retention must delete the old query_log row, got %d", n)
	}
	if n := countRows(t, env, "SELECT count(*) FROM search.query_log WHERE normalized_query = 'fresh query'"); n != 1 {
		t.Errorf("retention must keep the recent query_log row, got %d", n)
	}
	if n := countRows(t, env, "SELECT count(*) FROM search.behavior_events WHERE normalized_query = 'ancient query'"); n != 0 {
		t.Errorf("retention must delete the old behavior_events row, got %d", n)
	}
}

// TestIntegrationBehavioralReplayIdempotent replays a behavioral batch and proves
// the new tables dedupe on event_id (no double counting).
func TestIntegrationBehavioralReplayIdempotent(t *testing.T) {
	env := newTestEnv(t)
	now := time.Now()
	u := uuid.New()
	v := uuid.New()

	batch := []event.Envelope{
		submitted(now, "replay me", &u, "s1", true),
		clicked(now, "replay me", v, &u, "s1"),
	}

	first := ingest(t, env, batch...)
	if first.Accepted != 2 {
		t.Fatalf("first ingest should accept 2, got %+v", first)
	}
	// Contract shape: an all-success batch reports failed as an empty array.
	if first.Failed == nil {
		t.Errorf("failed must be a non-nil array")
	}

	second := ingest(t, env, batch...)
	if second.Accepted != 0 || second.Duplicates != 2 {
		t.Fatalf("replay should be all duplicates, got %+v", second)
	}
	if n := countRows(t, env, "SELECT count(*) FROM search.behavior_events"); n != 2 {
		t.Errorf("replay must not add behavior_events rows, got %d", n)
	}
	if n := countRows(t, env, "SELECT count(*) FROM search.query_log"); n != 1 {
		t.Errorf("replay must not add query_log rows, got %d", n)
	}
	if uc := countRows(t, env, "SELECT use_count FROM search.user_search_history WHERE user_id = $1 AND normalized_query = 'replay me'", u); uc != 1 {
		t.Errorf("replay must not bump use_count, got %d", uc)
	}
}

// TestIntegrationEngagementAndMeaningfulWatch drives the engagement rollup: a
// qualifying watch derives a meaningful_watch, feeds the watch projection, and
// folds into per-(query,video) engagement counters.
func TestIntegrationEngagementAndMeaningfulWatch(t *testing.T) {
	env := newTestEnv(t)
	now := time.Now()
	u := uuid.New()
	v := uuid.New()
	const q = "how to bake bread"

	ingest(t, env,
		play(now, v, q, &u, "s1", true),
		clicked(now.Add(time.Second), q, v, &u, "s1"),
		impression(now, q, v, "s1"),
		watch(now.Add(2*time.Second), v, 45, &u, "s1", true), // 45s ≥ 30s → meaningful
	)

	// Pass 1 derives the meaningful_watch and folds click/impression.
	runWorker(t, env, "engagement_rollup")
	if n := countRows(t, env, "SELECT count(*) FROM search.behavior_events WHERE type = 'video.meaningful_watch' AND video_id = $1", v); n != 1 {
		t.Fatalf("expected a derived meaningful_watch, got %d", n)
	}
	// Projection reflects play (+0.3) and meaningful watch (+1.0).
	var weight float64
	if err := env.store.Pool.QueryRow(context.Background(),
		"SELECT weight FROM search.user_watch_projection WHERE user_id = $1 AND video_id = $2", u, v).Scan(&weight); err != nil {
		t.Fatalf("projection weight: %v", err)
	}
	if weight < 1.0 {
		t.Errorf("projection weight = %v, want > 1.0 (play + meaningful watch)", weight)
	}

	// Pass 2 folds the derived meaningful_watch into engagement counters.
	runWorker(t, env, "engagement_rollup")
	var impr, clk, mw int64
	if err := env.store.Pool.QueryRow(context.Background(),
		"SELECT impressions, clicks, meaningful_watches FROM search.query_video_engagement WHERE normalized_query = $1 AND video_id = $2", q, v).
		Scan(&impr, &clk, &mw); err != nil {
		t.Fatalf("engagement row: %v", err)
	}
	if impr < 1 || clk < 1 || mw < 1 {
		t.Errorf("engagement counters impressions=%d clicks=%d meaningful=%d, want each ≥1", impr, clk, mw)
	}
}

// TestIntegrationSessionizerDerivesReformulation drives the sessionizer over
// settled query_log rows.
func TestIntegrationSessionizerDerivesReformulation(t *testing.T) {
	env := newTestEnv(t)
	// Two searches in one session, 30s apart, both old enough to be "settled"
	// (older than the 5-minute abandonment window).
	past := time.Now().Add(-10 * time.Minute)
	u := uuid.New()
	ingest(t, env,
		submitted(past, "cheap flights", &u, "sess-r", false),
		submitted(past.Add(30*time.Second), "cheap flights to paris", &u, "sess-r", false),
	)

	runWorker(t, env, "sessionizer")

	if n := countRows(t, env, "SELECT count(*) FROM search.behavior_events WHERE type = 'search.reformulated' AND normalized_query = 'cheap flights to paris'"); n != 1 {
		t.Errorf("expected a derived reformulation, got %d", n)
	}
	// The abandoned queries (no clicks/plays) are also derived.
	if n := countRows(t, env, "SELECT count(*) FROM search.behavior_events WHERE type = 'search.abandoned'"); n < 1 {
		t.Errorf("expected abandonment events for the un-engaged queries, got %d", n)
	}

	// Re-running is idempotent (deterministic derived ids).
	runWorker(t, env, "sessionizer")
	if n := countRows(t, env, "SELECT count(*) FROM search.behavior_events WHERE type = 'search.reformulated'"); n != 1 {
		t.Errorf("sessionizer re-run must be idempotent, got %d reformulations", n)
	}
}
