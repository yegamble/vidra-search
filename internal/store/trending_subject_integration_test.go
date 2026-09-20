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
	"math"
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
// ZSET at all. The score is the contribution count DECAYED to read time: each
// uncapped contribution bumps by exactly 1, then the sweeper decays it. Assert
// on it with assertTrendScore, never with ==.
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

// trendingMaxWallClockSeconds is the wall clock these tests are allowed to burn
// between the first TrendBump and the read that asserts on it. The whole test is
// ~0.1 s today, so 60 s is ~600x headroom for a badly loaded CI runner.
const trendingMaxWallClockSeconds = 60

// trendingDecayFloor is the fraction of an undecayed contribution that survives
// trendingMaxWallClockSeconds of decay:
//
//	2^(-60/21600) = 2^(-1/360) = 0.9980765...  ->  at most 0.19% of decay allowed.
var trendingDecayFloor = math.Exp2(-float64(trendingMaxWallClockSeconds) / float64(testTrendingHalfLifeSeconds))

// assertTrendScore checks a decayed ranking score against the number of
// contributions the item legitimately earned, as a two-sided bound.
//
// WHY a bound and not `score == want`: TrendBump and TrendSweep each stamp
// time.Now().Unix() — WHOLE seconds — and the sweep multiplies every member by
// 2^(-elapsed/halfLife). A run that ingests and sweeps inside one second sees
// elapsed=0 and an exactly integral score; a run that happens to straddle a
// second boundary sees elapsed=1 and a score of 2^(-1/21600) = 0.9999679. That
// 0.003% is wall clock, not ranking, and as an equality it reddened the required
// `integration` check on an unrelated x/text bump (Dependabot #48, 2026-09-14),
// roughly 1 run in 25. A security test that fails at random is a security test
// people learn to re-run without reading.
//
// The tolerance is spent in ONE direction only:
//
//   - UPPER bound `score <= want` stays EXACT and STRICT, because this is the
//     security assertion. Decay can only ever shrink a score, so no honest run
//     can exceed its undecayed contribution count — while the abuse this file
//     exists to catch (one subject rotating N session ids, or a broken cap)
//     scores 2x, 8x, 12x `want` and still fails as loudly as before.
//   - LOWER bound `score >= want*trendingDecayFloor` absorbs decay and nothing
//     else. A LOST contribution lands at `want-1` — for want=1 the item is gone
//     from the ZSET, and even at want=4 that is 3.0 against a floor of 3.99 —
//     so dropping an event still fails. Nothing but decay fits in the band.
//
// (Precedent: assertAggregateMatchesLedger in aggregates_retention_integration_test.go
// already compares the other decayed number, decayed_freq, within 1e-6.)
func assertTrendScore(t *testing.T, env *testEnv, domain, item string, want float64, why string) {
	t.Helper()
	score, ok := trendScore(t, env, domain, item)
	if !ok {
		t.Errorf("ranking score for %q is absent from the %q trend set, want ~%v: %s", item, domain, want, why)
		return
	}
	if score > want {
		t.Errorf("ranking score = %v, want at most %v — INFLATED: %s", score, want, why)
	}
	if floor := want * trendingDecayFloor; score < floor {
		t.Errorf("ranking score = %v, want at least %v (%v less up to %d s of 6 h-half-life decay): %s",
			score, floor, want, trendingMaxWallClockSeconds, why)
	}
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
	assertTrendScore(t, env, "q", nq, 1,
		"rotating the session header must not buy extra ranking weight")
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
	assertTrendScore(t, env, "v", vid.String(), 1,
		"rotating the session header must not inflate the home feed's video ranking")
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
	assertTrendScore(t, env, "q", nq, float64(n),
		"N distinct subjects must each buy their one contribution")
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
	assertTrendScore(t, env, "q", nq, float64(n),
		"subject-less traffic must not silently stop contributing")
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

	assertTrendScore(t, env, "q", nq, 1,
		"SEARCH_TREND_CAP_WINDOW must collapse every repeat from one subject into one contribution")
	total, err := env.cache.TrendTotal(context.Background(), "q", nq, 2)
	if err != nil {
		t.Fatalf("trend total: %v", err)
	}
	if total != repeats {
		t.Errorf("uncapped volume = %v, want %d: the cap bounds the RANKING, it must not hide volume from the Wilson gate", total, repeats)
	}
}
