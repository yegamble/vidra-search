//go:build integration

// Integration tests for the ANONYMOUS identity behind trending.
//
// Trending's two manipulation gates -- the distinct-user HLL floor and the
// per-subject contribution cap -- are only as trustworthy as the identity they
// count. That identity used to be `session_id`, which arrives in the
// client-controlled X-Vidra-Session header and is validated for UUID shape only:
// one machine rotating it presented N identities to both gates at once and took
// rank 1 of the published trending list from a single request loop. It is now
// core's server-derived `subject_id`, falling back to the session only where core
// emits no subject. These tests pin all four halves of that.
package store_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vidra/vidra-search/internal/event"
)

// anonPlay is anonSubmitted's counterpart for the "v" trending domain.
func anonPlay(occurredAt time.Time, videoID uuid.UUID, session, subject string) event.Envelope {
	p := map[string]any{"video_id": videoID.String(), "context": "home", "allow_history": false}
	if session != "" {
		p["session_id"] = session
	}
	if subject != "" {
		p["subject_id"] = subject
	}
	raw, _ := json.Marshal(p)
	return event.Envelope{EventID: uuid.New(), Type: event.TypeVideoPlayStarted, OccurredAt: occurredAt, SchemaVersion: 1, Payload: raw}
}

// trendScore returns an item's decayed ranking score, and whether it is in the
// ZSET at all. The score IS the contribution count here: each uncapped
// contribution bumps by exactly 1 and no half-life elapses within a test.
func trendScore(t *testing.T, env *testEnv, domain, item string) (float64, bool) {
	t.Helper()
	top, err := env.cache.TrendTop(context.Background(), domain, 100)
	if err != nil {
		t.Fatalf("trend top: %v", err)
	}
	for _, s := range top {
		if s.Item == item {
			return s.Score, true
		}
	}
	return 0, false
}

// trendDistinct returns the distinct-subject estimate trending's floor gate reads.
func trendDistinct(t *testing.T, env *testEnv, domain, item string) int64 {
	t.Helper()
	n, err := env.cache.TrendDistinctUsers(context.Background(), domain, item, 2)
	if err != nil {
		t.Fatalf("trend distinct: %v", err)
	}
	return n
}

// TestIntegrationRotatedSessionsCannotInflateTrending is THE security property
// for this surface. N events from ONE subject under N different session ids must
// contribute ONCE, not N times -- to the distinct-user HLL and to the ranking
// ZSET alike -- so the query never reaches the published trending list.
func TestIntegrationRotatedSessionsCannotInflateTrending(t *testing.T) {
	env := newTestEnv(t)
	now := time.Now()
	const nq = "buy cheap engagement"
	const subject = "subject-attacker-one"
	const n = 8

	var batch []event.Envelope
	for i := 0; i < n; i++ {
		batch = append(batch, anonSubmitted(now, nq, "rotated-session-"+string(rune('a'+i)), subject))
	}
	ingest(t, env, batch...)
	runWorker(t, env, "trending_sweeper")

	if d := trendDistinct(t, env, "q", nq); d != 1 {
		t.Errorf("distinct subjects = %d, want 1: %d rotated session ids from ONE server-derived subject are one contributor", d, n)
	}
	if score, ok := trendScore(t, env, "q", nq); !ok || score != 1 {
		t.Errorf("ranking score = %v (present=%v), want exactly 1: rotating the session header must not buy extra ranking weight", score, ok)
	}
	if set := env.cache.TrendingQuerySet(context.Background()); set[nq] != 0 {
		t.Errorf("a query pushed by ONE anonymous subject must not be published as trending, got %v", set)
	}
}

// TestIntegrationRotatedSessionsCannotInflateVideoTrending is the same property
// on the "v" domain. video.play_started carries subject_id too, and the home feed
// reads trend:v:top -- an untrusted identity here inflates the front page.
func TestIntegrationRotatedSessionsCannotInflateVideoTrending(t *testing.T) {
	env := newTestEnv(t)
	now := time.Now()
	vid := uuid.New()
	const subject = "subject-attacker-video"

	var batch []event.Envelope
	for i := 0; i < 8; i++ {
		batch = append(batch, anonPlay(now, vid, "rotated-session-"+string(rune('a'+i)), subject))
	}
	ingest(t, env, batch...)
	runWorker(t, env, "trending_sweeper")

	if d := trendDistinct(t, env, "v", vid.String()); d != 1 {
		t.Errorf("distinct subjects on the video domain = %d, want 1", d)
	}
	if score, _ := trendScore(t, env, "v", vid.String()); score != 1 {
		t.Errorf("video ranking score = %v, want exactly 1", score)
	}
	for _, s := range env.cache.TrendingVideos(context.Background()) {
		if s.Item == vid.String() {
			t.Errorf("a video pushed by ONE anonymous subject must not be published as trending")
		}
	}
}

// TestIntegrationDistinctSubjectsStillTrend is the other half: the fix must not
// degenerate into "anonymous traffic never trends" — that is most of a fresh
// instance's traffic, and refusing to count it would empty the surface.
func TestIntegrationDistinctSubjectsStillTrend(t *testing.T) {
	env := newTestEnv(t)
	now := time.Now()
	const nq = "world cup final"
	const n = 4

	var batch []event.Envelope
	for i := 0; i < n; i++ {
		s := string(rune('a' + i))
		batch = append(batch, anonSubmitted(now, nq, "session-"+s, "subject-"+s))
	}
	ingest(t, env, batch...)
	runWorker(t, env, "trending_sweeper")

	if d := trendDistinct(t, env, "q", nq); d != n {
		t.Errorf("distinct subjects = %d, want %d: N distinct subjects are N contributors", d, n)
	}
	if score, ok := trendScore(t, env, "q", nq); !ok || score != float64(n) {
		t.Errorf("ranking score = %v (present=%v), want %d", score, ok, n)
	}
	if _, ok := env.cache.TrendingQuerySet(context.Background())[nq]; !ok {
		t.Errorf("a query from %d distinct anonymous subjects must be published as trending", n)
	}
}

// TestIntegrationTrendingFallsBackToSessionWithoutSubject is the live-instance
// guard. An install whose core cannot derive a subject (a pre-0016 release, an
// unusual transport, no signing secret) emits none at all; those events must keep
// contributing exactly as before, or upgrading search silently zeroes that
// instance's trending with nothing in the logs to explain it.
func TestIntegrationTrendingFallsBackToSessionWithoutSubject(t *testing.T) {
	env := newTestEnv(t)
	now := time.Now()
	const nq = "self hosted video"
	const n = 4

	var batch []event.Envelope
	for i := 0; i < n; i++ {
		batch = append(batch, anonSubmitted(now, nq, "legacy-session-"+string(rune('a'+i)), ""))
	}
	ingest(t, env, batch...)
	runWorker(t, env, "trending_sweeper")

	if d := trendDistinct(t, env, "q", nq); d != n {
		t.Errorf("distinct = %d, want %d: with no subject to prefer, the session fallback must still count", d, n)
	}
	if score, ok := trendScore(t, env, "q", nq); !ok || score != float64(n) {
		t.Errorf("ranking score = %v (present=%v), want %d — subject-less traffic must not silently stop contributing", score, ok, n)
	}
	if _, ok := env.cache.TrendingQuerySet(context.Background())[nq]; !ok {
		t.Errorf("an install emitting no subject_id must still be able to trend")
	}
}

// TestIntegrationPerSubjectTrendCapAppliesUnderTheSubject proves the cap still
// bites under the new identity: one subject searching repeatedly inside one cap
// window contributes ONE ranking bump, while the uncapped volume counter still
// records every event (that raw volume is what the Wilson gate divides by).
func TestIntegrationPerSubjectTrendCapAppliesUnderTheSubject(t *testing.T) {
	env := newTestEnv(t)
	now := time.Now()
	const nq = "same subject many searches"
	const repeats = 12

	var batch []event.Envelope
	for i := 0; i < repeats; i++ {
		// One subject, one stable session, many searches inside the cap window.
		batch = append(batch, anonSubmitted(now, nq, "steady-session", "subject-steady"))
	}
	ingest(t, env, batch...)
	runWorker(t, env, "trending_sweeper")

	if score, _ := trendScore(t, env, "q", nq); score != 1 {
		t.Errorf("ranking score = %v, want 1: SEARCH_TREND_CAP_WINDOW must collapse %d contributions from one subject", score, repeats)
	}
	total, err := env.cache.TrendTotal(context.Background(), "q", nq, 2)
	if err != nil {
		t.Fatalf("trend total: %v", err)
	}
	if total != repeats {
		t.Errorf("uncapped volume = %v, want %d: the cap bounds the RANKING, it must not hide volume from the Wilson gate", total, repeats)
	}
}
