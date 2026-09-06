//go:build integration

// Integration tests for the k-anonymity floor on CO-VISITATION NEIGHBOURS.
//
// The neighbour rebuild used to filter on `score > 0` and nothing else, so one
// person's single session published a public "watch this next" association
// between every pair of videos they happened to open: the A13 proof watched
// C1 → P → U once and got three neighbour edges visible to every viewer. The
// owner ruled that this table carries the SAME floor autosuggest and trending
// carry — `minimum_query_user_count`, counting the same identity (the account id
// when signed in, else core's server-derived subject_id, else the client's
// session id).
//
// These tests pin the four properties that make that floor mean something:
// a pair AT the floor is still published with its score unchanged, a pair below
// it is absent, one subject repeating a pair in many sessions counts ONCE, and
// the floor is re-decided from scratch every pass so an edge whose support ages
// out of the retained ledger disappears on the next rollup.
package store_test

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vidra/vidra-search/internal/event"
)

// anonClicked is anonPlay's (trending_subject_integration_test.go)
// search.result_clicked twin — the co_search half of the blend. Both carry core's
// server-derived subject_id ALONGSIDE the client-supplied session id, the shape
// vidra-core puts on the wire for an anonymous visitor; an empty subject is how a
// pre-subject core (or a request whose address could not be derived) presents,
// with the field simply absent and the floor falling back to session_id.
func anonClicked(occurredAt time.Time, query string, videoID uuid.UUID, session, subject string) event.Envelope {
	p := map[string]any{"query": query, "video_id": videoID.String()}
	if session != "" {
		p["session_id"] = session
	}
	if subject != "" {
		p["subject_id"] = subject
	}
	return behEnv(event.TypeSearchResultClicked, occurredAt, p)
}

// neighborEdge reads the published covis-v1 score for the directed edge a → b.
func neighborEdge(t *testing.T, env *testEnv, a, b uuid.UUID) (float64, bool) {
	t.Helper()
	var n int64
	var score float64
	if err := env.store.Pool.QueryRow(context.Background(),
		`SELECT count(*), COALESCE(max(score), 0)::double precision
		   FROM search.item_neighbors WHERE video_id = $1 AND neighbor_id = $2`, a, b).Scan(&n, &score); err != nil {
		t.Fatalf("read neighbor edge: %v", err)
	}
	return score, n > 0
}

// coOccurrences reads the cumulative counter for an unordered pair, whichever way
// round it was normalized.
func coOccurrences(t *testing.T, env *testEnv, table string, a, b uuid.UUID) int64 {
	t.Helper()
	return countRows(t, env, `SELECT COALESCE(max(count), 0) FROM search.`+table+
		` WHERE (video_a = $1 AND video_b = $2) OR (video_a = $2 AND video_b = $1)`, a, b)
}

// TestIntegrationCovisFloorPublishesAtTheFloorAndDropsBelowIt is the ruling in
// one fixture: the A13 pair that three distinct people co-watched keeps its
// EXACT score, and the three edges one person's single session produced are gone.
func TestIntegrationCovisFloorPublishesAtTheFloorAndDropsBelowIt(t *testing.T) {
	env := newTestEnv(t)
	now := time.Now()

	// The A13 score fixture: three distinct signed-in users each watch A1 then
	// A2, one session each. cooc = 3, totI = totJ = 3, so the blend is
	// 0.7 · (3/sqrt(3·3)) · (3/(3+10)) = 0.7 · 3/13 = 0.16154.
	a1, a2 := uuid.New(), uuid.New()
	for i := 0; i < 3; i++ {
		u := uuid.New()
		sess := fmt.Sprintf("kf-a-%d", i)
		ingest(t, env,
			play(now, a1, "", &u, sess, false),
			play(now.Add(time.Second), a2, "", &u, sess, false))
	}

	// The A13 proof: ONE user, ONE session, C1 → P → U. Three pairs, each
	// supported by exactly one subject — three public edges off one person's
	// browsing.
	c1, pv, uv := uuid.New(), uuid.New(), uuid.New()
	dave := uuid.New()
	ingest(t, env,
		play(now, c1, "", &dave, "kf-dave", false),
		play(now.Add(time.Second), pv, "", &dave, "kf-dave", false),
		play(now.Add(2*time.Second), uv, "", &dave, "kf-dave", false))

	runWorker(t, env, "covis_rollup")

	// The floor is a PUBLICATION gate, not a counting one: the cumulative
	// counters still saw every pair, including Dave's three.
	if n := countRows(t, env, `SELECT count(*) FROM search.co_watch`); n != 4 {
		t.Fatalf("co_watch pairs = %d, want 4 (A1-A2 plus Dave's three)", n)
	}

	score, ok := neighborEdge(t, env, a1, a2)
	if !ok {
		t.Fatalf("A1→A2 was co-watched by three distinct users — at the floor it must still be published")
	}
	if math.Abs(score-0.16153846) > 1e-6 {
		t.Errorf("A1→A2 score = %.8f, want 0.16154 (0.7 · 3/13) — the floor gates which edges publish, "+
			"it must not change the score formula", score)
	}
	if _, ok := neighborEdge(t, env, a2, a1); !ok {
		t.Errorf("the reverse direction A2→A1 must be published too")
	}

	for _, pair := range [][2]uuid.UUID{{c1, pv}, {c1, uv}, {pv, uv}, {pv, c1}, {uv, c1}, {uv, pv}} {
		if _, ok := neighborEdge(t, env, pair[0], pair[1]); ok {
			t.Errorf("one person's single session must not publish the neighbour edge %s→%s", pair[0], pair[1])
		}
	}
}

// TestIntegrationCovisFloorCountsOneAnonymousSubjectOnce is THE security
// property, and it is the co-visitation twin of
// TestIntegrationRotatedSessionsOneSubjectCountOnce. One machine rotating
// X-Vidra-Session presents N well-formed session ids but ONE server-derived
// subject, so the floor must see one — otherwise a single request loop publishes
// any association it likes into everybody's "watch this next".
func TestIntegrationCovisFloorCountsOneAnonymousSubjectOnce(t *testing.T) {
	env := newTestEnv(t)
	now := time.Now()

	// One subject, six sessions, six co-visits of the same pair.
	b1, b2 := uuid.New(), uuid.New()
	for i := 0; i < 6; i++ {
		at := now.Add(time.Duration(i) * time.Minute)
		sess := fmt.Sprintf("kf-rot-%d", i)
		ingest(t, env,
			anonPlay(at, b1, sess, "subject-fake-rotator"),
			anonPlay(at.Add(time.Second), b2, sess, "subject-fake-rotator"))
	}

	// Control: three DIFFERENT subjects, one session each, same shape otherwise.
	c1, c2 := uuid.New(), uuid.New()
	for i := 0; i < 3; i++ {
		at := now.Add(time.Duration(i) * time.Minute)
		sess := fmt.Sprintf("kf-sub-%d", i)
		subj := fmt.Sprintf("subject-fake-%d", i)
		ingest(t, env,
			anonPlay(at, c1, sess, subj),
			anonPlay(at.Add(time.Second), c2, sess, subj))
	}

	runWorker(t, env, "covis_rollup")

	if n := coOccurrences(t, env, "co_watch", b1, b2); n != 6 {
		t.Fatalf("co_watch(B1,B2) = %d, want 6 — the counter counts visits, only the floor counts people", n)
	}
	if _, ok := neighborEdge(t, env, b1, b2); ok {
		t.Errorf("six co-visits by ONE subject across six sessions must count as one and stay below the floor")
	}
	if _, ok := neighborEdge(t, env, c1, c2); !ok {
		t.Errorf("three distinct subjects must clear the floor (control: the fixture shape is otherwise identical)")
	}
}

// anonWatch is an ANONYMOUS video.watch_progress envelope — the event the
// engagement rollup derives video.meaningful_watch from.
func anonWatch(occurredAt time.Time, videoID uuid.UUID, positionSeconds float64, session, subject string) event.Envelope {
	p := map[string]any{"video_id": videoID.String(), "position_seconds": positionSeconds, "allow_history": false}
	if session != "" {
		p["session_id"] = session
	}
	if subject != "" {
		p["subject_id"] = subject
	}
	return behEnv(event.TypeVideoWatchProgress, occurredAt, p)
}

// TestIntegrationCovisFloorCountsSubjectsOnDerivedMeaningfulWatches is a FORWARD
// GUARD on the one event type the floor counts that this service synthesises
// itself. video.meaningful_watch is derived by DeriveMeaningfulWatch, so its
// props are whatever that jsonb_build_object builds; while that object dropped
// subject_id, a derived row fell through to the client-controlled session_id and
// an anonymous actor rotating X-Vidra-Session presented as N distinct "people"
// behind a pair whose play_started half correctly counted one.
//
// State plainly what this does NOT prove: that shape is not reachable through
// today's vidra-core. video.watch_progress is emitted only from the
// authenticated PUT /videos/:id/watch-progress route (always with a user_id) and
// is not on the public POST /search/events allowlist, so no anonymous
// watch_progress exists and the floor counts the user_id. This fixture therefore
// drives the search service directly, the way core WOULD if that route ever
// accepted an anonymous beacon or the type joined the allowlist — which is
// exactly when a synthesised row silently counting a forgeable identity would
// otherwise ship with nothing failing.
func TestIntegrationCovisFloorCountsSubjectsOnDerivedMeaningfulWatches(t *testing.T) {
	env := newTestEnv(t)
	now := time.Now()

	// One subject, six sessions, watch_progress ONLY — so every co_watch pair
	// here comes from a derived meaningful_watch, not from a play_started.
	d1, d2 := uuid.New(), uuid.New()
	for i := 0; i < 6; i++ {
		at := now.Add(time.Duration(i) * time.Minute)
		sess := fmt.Sprintf("kf-mw-rot-%d", i)
		ingest(t, env,
			anonWatch(at, d1, 60, sess, "subject-fake-mw-rotator"),
			anonWatch(at.Add(time.Second), d2, 60, sess, "subject-fake-mw-rotator"))
	}
	// Control: three distinct subjects, same shape.
	e1, e2 := uuid.New(), uuid.New()
	for i := 0; i < 3; i++ {
		at := now.Add(time.Duration(i) * time.Minute)
		sess := fmt.Sprintf("kf-mw-sub-%d", i)
		subj := fmt.Sprintf("subject-fake-mw-%d", i)
		ingest(t, env,
			anonWatch(at, e1, 60, sess, subj),
			anonWatch(at.Add(time.Second), e2, 60, sess, subj))
	}

	runWorker(t, env, "engagement_rollup") // derives the meaningful_watch rows
	if n := countRows(t, env, `SELECT count(*) FROM search.behavior_events WHERE type = 'video.meaningful_watch'`); n != 18 {
		t.Fatalf("derived meaningful_watch rows = %d, want 18", n)
	}
	runWorker(t, env, "covis_rollup")

	if n := coOccurrences(t, env, "co_watch", d1, d2); n != 6 {
		t.Fatalf("co_watch(D1,D2) = %d, want 6 — the pairs must come from the derived rows", n)
	}
	if _, ok := neighborEdge(t, env, d1, d2); ok {
		t.Errorf("a derived meaningful_watch must carry the subject it was derived from: " +
			"six sessions of ONE subject must count as one and stay below the floor")
	}
	if _, ok := neighborEdge(t, env, e1, e2); !ok {
		t.Errorf("three distinct subjects watching meaningfully must clear the floor")
	}
}

// TestIntegrationCovisFloorAppliesToCoSearchAndTheBlend pins that BOTH blended
// sources are floored, and floored independently: a below-floor co-search
// contributes nothing, so it can neither publish an edge on its own nor inflate
// an edge the co-watch half earned.
func TestIntegrationCovisFloorAppliesToCoSearchAndTheBlend(t *testing.T) {
	env := newTestEnv(t)
	now := time.Now()

	// (1) Support from ONE subject's co-search only.
	s1, s2 := uuid.New(), uuid.New()
	ingest(t, env,
		anonClicked(now, "quolbex drift", s1, "kf-cs-solo", "subject-fake-solo"),
		anonClicked(now.Add(time.Second), "quolbex drift", s2, "kf-cs-solo", "subject-fake-solo"))

	// (2) Support from THREE subjects' co-search.
	m1, m2 := uuid.New(), uuid.New()
	for i := 0; i < 3; i++ {
		at := now.Add(time.Duration(i) * time.Minute)
		ingest(t, env,
			anonClicked(at, "traskil mirvane", m1, fmt.Sprintf("kf-cs-%d", i), fmt.Sprintf("subject-fake-cs-%d", i)),
			anonClicked(at.Add(time.Second), "traskil mirvane", m2, fmt.Sprintf("kf-cs-%d", i), fmt.Sprintf("subject-fake-cs-%d", i)))
	}

	// (3) The blend: co-watch clears the floor (three users), co-search does not
	// (one user). The published score must be the co-watch term ALONE.
	w1, w2 := uuid.New(), uuid.New()
	for i := 0; i < 3; i++ {
		u := uuid.New()
		sess := fmt.Sprintf("kf-w-%d", i)
		ingest(t, env,
			play(now, w1, "", &u, sess, false),
			play(now.Add(time.Second), w2, "", &u, sess, false))
	}
	solo := uuid.New()
	ingest(t, env,
		clicked(now, "plexoid wondrix", w1, &solo, "kf-w-solo"),
		clicked(now.Add(time.Second), "plexoid wondrix", w2, &solo, "kf-w-solo"))

	runWorker(t, env, "covis_rollup")

	if n := coOccurrences(t, env, "co_search", s1, s2); n != 1 {
		t.Fatalf("co_search(S1,S2) = %d, want 1", n)
	}
	if _, ok := neighborEdge(t, env, s1, s2); ok {
		t.Errorf("an edge whose ONLY support is one subject's co-search must not appear via the blend")
	}

	// 0.3 · (3/sqrt(3·3)) · (3/(3+10)) = 0.3 · 3/13 = 0.06923.
	msc, ok := neighborEdge(t, env, m1, m2)
	if !ok {
		t.Fatalf("co-search support from three subjects must clear the floor")
	}
	if math.Abs(msc-0.06923077) > 1e-6 {
		t.Errorf("M1→M2 score = %.8f, want 0.06923 (0.3 · 3/13)", msc)
	}

	// Co-watch alone: 0.7 · 3/13 = 0.16154. If the below-floor co-search leaked
	// into the blend this would be 0.16154 + 0.3 · (1/sqrt(1·1)) · 1/11 = 0.18881.
	wsc, ok := neighborEdge(t, env, w1, w2)
	if !ok {
		t.Fatalf("W1→W2's co-watch support clears the floor, so the edge must publish")
	}
	if math.Abs(wsc-0.16153846) > 1e-6 {
		t.Errorf("W1→W2 score = %.8f, want 0.16154 — a below-floor co-search must contribute nothing to the blend", wsc)
	}
}

// TestIntegrationCovisFloorDropsEdgeWhenRetentionPrunesItsSupport pins that the
// rebuild stays a FROM-SCRATCH rebuild once the floor is part of it. The
// cumulative co_watch counter is never pruned, so support has to be re-read from
// the retained event ledger every pass — otherwise an edge that three people
// earned in 2026 stays published forever on evidence the instance deleted.
func TestIntegrationCovisFloorDropsEdgeWhenRetentionPrunesItsSupport(t *testing.T) {
	env := newTestEnv(t)
	now := time.Now()
	old := now.Add(-100 * 24 * time.Hour) // past the 90-day retention default

	r1, r2 := uuid.New(), uuid.New()
	for i := 0; i < 2; i++ {
		at := old.Add(time.Duration(i) * time.Minute)
		ingest(t, env,
			anonPlay(at, r1, fmt.Sprintf("kf-old-%d", i), fmt.Sprintf("subject-fake-old-%d", i)),
			anonPlay(at.Add(time.Second), r2, fmt.Sprintf("kf-old-%d", i), fmt.Sprintf("subject-fake-old-%d", i)))
	}
	ingest(t, env,
		anonPlay(now, r1, "kf-fresh", "subject-fake-fresh"),
		anonPlay(now.Add(time.Second), r2, "kf-fresh", "subject-fake-fresh"))

	runWorker(t, env, "covis_rollup")
	if _, ok := neighborEdge(t, env, r1, r2); !ok {
		t.Fatalf("three subjects must publish the edge before retention runs")
	}

	runWorker(t, env, "retention")
	if n := countRows(t, env, `SELECT count(*) FROM search.behavior_events`); n != 2 {
		t.Fatalf("behavior_events after retention = %d, want 2 (only the fresh subject's pair survives)", n)
	}
	// The counter is untouched — which is exactly why the floor cannot be read
	// out of it.
	if n := coOccurrences(t, env, "co_watch", r1, r2); n != 3 {
		t.Fatalf("co_watch(R1,R2) after retention = %d, want 3 (cumulative counters are not pruned)", n)
	}

	runWorker(t, env, "covis_rollup")
	if _, ok := neighborEdge(t, env, r1, r2); ok {
		t.Errorf("once retention leaves fewer than the floor's subjects behind, the edge must disappear on the next rollup")
	}
}

// TestIntegrationCovisFloorLeavesTheNormalizationMassIntact pins the choice that
// is easiest to get wrong and hardest to notice: the floor gates PUBLICATION, not
// counting, so a dropped pair still contributes its co-occurrence mass to the
// cosine's denominator for the pairs that survive.
//
// Filtering the totals as well looks tidier and is a silent relevance change: it
// re-scales every surviving neighbour of any item that had a below-floor pair,
// upward, in proportion to how much private browsing that item attracted. The
// fixture separates the two readings numerically — X is co-watched with Y by three
// users (published) and with Z by one (dropped), so totX is 4 with the mass intact
// and 3 without.
func TestIntegrationCovisFloorLeavesTheNormalizationMassIntact(t *testing.T) {
	env := newTestEnv(t)
	now := time.Now()

	x, y, z := uuid.New(), uuid.New(), uuid.New()
	for i := 0; i < 3; i++ {
		u := uuid.New()
		sess := fmt.Sprintf("kf-mass-%d", i)
		ingest(t, env,
			play(now, x, "", &u, sess, false),
			play(now.Add(time.Second), y, "", &u, sess, false))
	}
	loner := uuid.New()
	ingest(t, env,
		play(now, x, "", &loner, "kf-mass-solo", false),
		play(now.Add(time.Second), z, "", &loner, "kf-mass-solo", false))

	runWorker(t, env, "covis_rollup")

	if _, ok := neighborEdge(t, env, x, z); ok {
		t.Errorf("the one-person X-Z pair must not be published")
	}
	score, ok := neighborEdge(t, env, x, y)
	if !ok {
		t.Fatalf("the three-person X-Y pair must be published")
	}
	// totX = 3 (X-Y) + 1 (X-Z, dropped but still counted mass), totY = 3:
	// 0.7 · (3/sqrt(4·3)) · (3/13) = 0.13990.
	// Had the floor also filtered the totals, totX would be 3 and this would read
	// 0.16154 — the score X-Y would have had if Z had never been watched.
	if math.Abs(score-0.13989560) > 1e-6 {
		t.Errorf("X→Y score = %.8f, want 0.13990 (totX = 4, mass intact). "+
			"0.16154 here means the floor was applied to the normalization totals too, "+
			"which silently re-scales every surviving neighbour", score)
	}
}
