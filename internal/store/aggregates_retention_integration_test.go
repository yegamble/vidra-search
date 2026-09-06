//go:build integration

// Integration tests for the ruling that THE POPULARITY AGGREGATES RESPECT EVENT
// RETENTION — the half vidra-search#36 left behind.
//
// #36 took the cumulative co-visitation counters out of the co-visitation score
// because nothing ever pruned them: a co-visit kept contributing long after
// retention deleted both of its events, and a user's purge could not reach it.
// #37 then made "Clear all" delete the caller's rows outright and wrote down,
// per surface, how fast a deletion reaches it. Two aggregates failed that table:
//
//   - `query_aggregates.total_count` / `decayed_freq` — folded forward by the
//     rollup's cursor (`total_count + delta`, decay-then-increment) and never
//     recomputed. Only `distinct_users` / `suggestible` were. `decayed_freq` is
//     what ORDERS autosuggest, so a query kept the rank its deleted searches
//     bought it.
//   - `query_video_engagement` — cumulative impression/click/meaningful-watch
//     counters folded forward by cursor and never pruned: the exact shape #36
//     removed from co-visitation. It is read live (SearchAdvancedRecall recalls
//     on `clicks > 0` and feeds CTR / meaningful-watch-rate into stage-2).
//
// These tests pin the same property #36 pinned, for those two: what is stored is
// what recomputing from the CURRENTLY RETAINED ledger gives, and nothing else.
// Every "want" is computed here from the ledger with the predicate spelled out —
// the same second-opinion convention `ledgerCoVisits` uses, so the tests can
// disagree with the production SQL instead of only proving it agrees with itself.
package store_test

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vidra/vidra-search/internal/event"
	"github.com/vidra/vidra-search/internal/suggest"
)

// testQueryHalfLifeSeconds is worker.Config's default QAC half-life (7 days),
// which the test env inherits by leaving QueryHalfLifeSeconds zero. It is also
// the shipped default (SEARCH_QUERY_HALF_LIFE_HOURS=168).
const testQueryHalfLifeSeconds = 7 * 24 * 3600.0

// testRetentionDays is worker.Config's default EVENT_RETENTION_DAYS, which the
// rollup and the daily re-evaluation both use as their window.
const testRetentionDays = 90

// ledgerQueryTotals recomputes, straight from the retained `query_log`, the two
// numbers `query_aggregates` is supposed to hold for a query: how many searches
// this instance still has evidence of, and their recency-weighted sum anchored
// on the newest surviving one. The window is production's — rows older than
// EVENT_RETENTION_DAYS do not count even before the daily retention worker
// physically deletes them.
//
// Spelled out here rather than calling the shipped query on purpose: this is the
// tests' independent second opinion.
func ledgerQueryTotals(t *testing.T, env *testEnv, nq string) (total int64, freq float64) {
	t.Helper()
	const q = `
	WITH retained AS (
	    SELECT submitted_at FROM search.query_log
	     WHERE normalized_query = $1
	       AND submitted_at >= now() - make_interval(days => $2::int)
	), anchor AS (
	    SELECT max(submitted_at) AS last_seen FROM retained
	)
	SELECT count(*)::bigint,
	       COALESCE(sum(power(2, - GREATEST(0, EXTRACT(EPOCH FROM (a.last_seen - r.submitted_at)))
	                             / $3::double precision)), 0)::double precision
	  FROM retained r CROSS JOIN anchor a`
	if err := env.store.Pool.QueryRow(context.Background(), q, nq, testRetentionDays, testQueryHalfLifeSeconds).
		Scan(&total, &freq); err != nil {
		t.Fatalf("ledgerQueryTotals(%q): %v", nq, err)
	}
	return total, freq
}

// storedQueryTotals reads what the aggregate row actually holds.
func storedQueryTotals(t *testing.T, env *testEnv, nq string) (total int64, freq float64) {
	t.Helper()
	if err := env.store.Pool.QueryRow(context.Background(),
		`SELECT total_count, decayed_freq FROM search.query_aggregates WHERE normalized_query = $1`, nq).
		Scan(&total, &freq); err != nil {
		t.Fatalf("storedQueryTotals(%q): %v", nq, err)
	}
	return total, freq
}

// assertAggregateMatchesLedger is the whole ruling in one assertion.
func assertAggregateMatchesLedger(t *testing.T, env *testEnv, nq, when string) {
	t.Helper()
	wantTotal, wantFreq := ledgerQueryTotals(t, env, nq)
	gotTotal, gotFreq := storedQueryTotals(t, env, nq)
	if gotTotal != wantTotal {
		t.Errorf("%s: %q total_count = %d, want %d — the number recomputed from the retained query_log. "+
			"A stored count larger than the ledger is the accumulator still counting searches this instance has deleted",
			when, nq, gotTotal, wantTotal)
	}
	if math.Abs(gotFreq-wantFreq) > 1e-6 {
		t.Errorf("%s: %q decayed_freq = %.8f, want %.8f (recomputed from the retained query_log at a 168 h half-life). "+
			"decayed_freq is autosuggest's sort key, so a stale one keeps the rank the deleted searches bought",
			when, nq, gotFreq, wantFreq)
	}
}

// searchedByAt submits a query once per distinct user at a chosen time.
func searchedByAt(t *testing.T, env *testEnv, at time.Time, query string, users int) {
	t.Helper()
	var batch []event.Envelope
	for i := 0; i < users; i++ {
		u := uuid.New()
		batch = append(batch, submitted(at, query, &u, "", false))
	}
	ingest(t, env, batch...)
}

// TestIntegrationQueryAggregateCountsComeOnlyFromTheRetainedLedger is the SC1
// ruling stated as a single property, in the two shapes that break it: rows that
// have aged PAST the retention window but not yet been swept, and rows the
// retention worker has physically deleted.
func TestIntegrationQueryAggregateCountsComeOnlyFromTheRetainedLedger(t *testing.T) {
	env := newTestEnv(t)
	now := time.Now()
	old := now.Add(-100 * 24 * time.Hour) // past the 90-day retention default
	const nq = "retained aggregate demo"

	// Three searches inside the window, three outside it.
	searchedByAt(t, env, now, nq, 3)
	searchedByAt(t, env, old, nq, 3)

	runWorker(t, env, "aggregates_rollup")

	// The window is the rollup's own (`submitted_at >= now() - retention`), so
	// the aged rows must not count even though they are still on disk.
	if total, _ := ledgerQueryTotals(t, env, nq); total != 3 {
		t.Fatalf("fixture: the retained window must hold exactly the 3 recent searches, holds %d", total)
	}
	assertAggregateMatchesLedger(t, env, nq, "after the first rollup")

	// Now the retention worker physically deletes the aged rows, and one further
	// genuine search arrives so the rollup has a batch to fold.
	runWorker(t, env, "retention")
	if n := countRows(t, env, "SELECT count(*) FROM search.query_log WHERE normalized_query = $1", nq); n != 3 {
		t.Fatalf("retention must leave exactly the 3 in-window rows, left %d", n)
	}
	searchedByAt(t, env, now, nq, 1)
	runWorker(t, env, "aggregates_rollup")

	if total, _ := ledgerQueryTotals(t, env, nq); total != 4 {
		t.Fatalf("fixture: after retention plus one search the ledger must hold 4 rows, holds %d", total)
	}
	assertAggregateMatchesLedger(t, env, nq, "after retention and a further rollup")
}

// TestIntegrationQuietQueryCountsAreRecomputedWithinADay is the promise the
// frontend makes about a clear-all — "the anonymous popularity totals this site
// keeps are recomputed without you within a day" — for a query that receives NO
// further traffic. The 1-minute rollup only revisits queries in its batch (the
// join is INNER), so a query that goes quiet is reached only by the daily
// `suggestible_reeval` pass. Before this change that pass moved `suggestible` and
// `distinct_users` and left the volume counters untouched for ever.
func TestIntegrationQuietQueryCountsAreRecomputedWithinADay(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	now := time.Now()
	const nq = "quiet aggregate demo"

	clearer := uuid.New()
	batch := []event.Envelope{
		submitted(now, nq, &clearer, "qa-1", true),
		submitted(now.Add(time.Second), nq, &clearer, "qa-1", true),
	}
	for i := 0; i < 3; i++ {
		u := uuid.New()
		batch = append(batch, submitted(now, nq, &u, "", false))
	}
	ingest(t, env, batch...)
	runWorker(t, env, "aggregates_rollup")

	if total, _ := storedQueryTotals(t, env, nq); total != 5 {
		t.Fatalf("fixture: five searches must roll up to total_count 5, got %d", total)
	}
	assertAggregateMatchesLedger(t, env, nq, "before the clear")

	// The clearer presses "Clear all". Their two query_log rows go (#37), and no
	// further traffic ever arrives for this string.
	if err := env.history.ClearAll(ctx, clearer); err != nil {
		t.Fatalf("ClearAll: %v", err)
	}
	if n := countRows(t, env, "SELECT count(*) FROM search.query_log WHERE normalized_query = $1", nq); n != 3 {
		t.Fatalf("fixture: clear-all must leave the other three searchers' 3 rows, left %d", n)
	}

	// The rollup cannot help: there is no new traffic, so this string is not in
	// its batch. The daily pass is what has to close the promise.
	runWorker(t, env, "aggregates_rollup")
	runWorker(t, env, "suggestible_reeval")

	if total, _ := ledgerQueryTotals(t, env, nq); total != 3 {
		t.Fatalf("fixture: the ledger must hold 3 rows after the clear, holds %d", total)
	}
	assertAggregateMatchesLedger(t, env, nq, "after clear-all and the daily re-evaluation")
}

// TestIntegrationAutosuggestOrderFollowsTheRetainedLedger is why the counters
// matter at all: `decayed_freq` is the ORDER of the aggregate suggestion stream.
// A query whose evidence has aged out of the window must not outrank one whose
// evidence survives.
func TestIntegrationAutosuggestOrderFollowsTheRetainedLedger(t *testing.T) {
	env := newTestEnv(t)
	now := time.Now()
	old := now.Add(-100 * 24 * time.Hour)
	const steady = "orderdemo steady"
	const faded = "orderdemo faded"

	// steady: four searchers, all inside the window.
	searchedByAt(t, env, now, steady, 4)
	// faded: three searchers inside the window and three whose searches have aged
	// past retention. Six accumulated hits beat steady's four; three surviving
	// ones do not.
	searchedByAt(t, env, now, faded, 3)
	searchedByAt(t, env, old, faded, 3)

	runWorker(t, env, "aggregates_rollup")

	assertAggregateMatchesLedger(t, env, steady, "aggregate stream order")
	assertAggregateMatchesLedger(t, env, faded, "aggregate stream order")

	resp := env.sugg.Suggest(context.Background(), suggest.Request{Query: "orderdemo", Limit: 20})
	texts := suggestionTexts(resp.Suggestions)
	iSteady, iFaded := -1, -1
	for i, s := range texts {
		switch s {
		case steady:
			iSteady = i
		case faded:
			iFaded = i
		}
	}
	if iSteady < 0 || iFaded < 0 {
		t.Fatalf("both queries must be suggestible (three-plus distinct searchers each); got %v", texts)
	}
	if iSteady > iFaded {
		t.Errorf("autosuggest ranked %q above %q (%v). decayed_freq must be recomputed from the retained "+
			"query_log, so searches this instance no longer holds cannot buy a completion its rank", faded, steady, texts)
	}
}

// --- query_video_engagement (SC2) ---

// ledgerEngagement recomputes the per-(query, video) counters straight from the
// retained `behavior_events`, with the fold's own predicate spelled out.
func ledgerEngagement(t *testing.T, env *testEnv, nq string, video uuid.UUID) (impressions, clicks, watches int64) {
	t.Helper()
	const q = `
	SELECT count(*) FILTER (WHERE type = 'video.impression')::bigint,
	       count(*) FILTER (WHERE type = 'search.result_clicked')::bigint,
	       count(*) FILTER (WHERE type = 'video.meaningful_watch')::bigint
	  FROM search.behavior_events
	 WHERE normalized_query = $1 AND video_id = $2
	   AND type IN ('video.impression', 'search.result_clicked', 'video.meaningful_watch')`
	if err := env.store.Pool.QueryRow(context.Background(), q, nq, video).Scan(&impressions, &clicks, &watches); err != nil {
		t.Fatalf("ledgerEngagement: %v", err)
	}
	return
}

// storedEngagement reads the aggregate row (zeroes when there is none, which is
// the correct answer once the evidence is gone).
func storedEngagement(t *testing.T, env *testEnv, nq string, video uuid.UUID) (impressions, clicks, watches int64) {
	t.Helper()
	if err := env.store.Pool.QueryRow(context.Background(),
		`SELECT COALESCE(max(impressions), 0)::bigint, COALESCE(max(clicks), 0)::bigint,
		        COALESCE(max(meaningful_watches), 0)::bigint
		   FROM search.query_video_engagement WHERE normalized_query = $1 AND video_id = $2`, nq, video).
		Scan(&impressions, &clicks, &watches); err != nil {
		t.Fatalf("storedEngagement: %v", err)
	}
	return
}

func assertEngagementMatchesLedger(t *testing.T, env *testEnv, nq string, video uuid.UUID, when string) {
	t.Helper()
	wi, wc, ww := ledgerEngagement(t, env, nq, video)
	gi, gc, gw := storedEngagement(t, env, nq, video)
	if gi != wi || gc != wc || gw != ww {
		t.Errorf("%s: query_video_engagement(%q) = impressions %d / clicks %d / watches %d, "+
			"want %d / %d / %d recomputed from the retained behavior_events. Counters larger than the "+
			"ledger are the cursor fold still counting events this instance has deleted — the same shape "+
			"vidra-search#36 removed from co-visitation", when, nq, gi, gc, gw, wi, wc, ww)
	}
}

// TestIntegrationEngagementCountersRespectRetention is SC2: an event that
// retention deletes stops contributing to `query_video_engagement` on the next
// pass. That table is read live — SearchAdvancedRecall recalls candidates on
// `clicks > 0` and feeds impressions/clicks/watches into the stage-2 CTR and
// meaningful-watch-rate features — so a counter retention cannot reach keeps a
// deleted click recalling and ranking a video for ever.
func TestIntegrationEngagementCountersRespectRetention(t *testing.T) {
	env := newTestEnv(t)
	now := time.Now()
	old := now.Add(-100 * 24 * time.Hour)
	v := uuid.New()
	const nq = "engagement retention demo"

	u := uuid.New()
	ingest(t, env,
		impression(old, nq, v, "er-old"),
		impression(old.Add(time.Second), nq, v, "er-old"),
		clicked(old.Add(2*time.Second), nq, v, &u, "er-old"),
		impression(now, nq, v, "er-new"),
		clicked(now.Add(time.Second), nq, v, &u, "er-new"),
	)

	runWorker(t, env, "engagement_rollup")
	assertEngagementMatchesLedger(t, env, nq, v, "before retention")

	// Retention deletes the three aged events. No new event follows, so a
	// cursor-driven fold has nothing to do and the counters stand still.
	runWorker(t, env, "retention")
	if n := countRows(t, env, "SELECT count(*) FROM search.behavior_events WHERE normalized_query = $1", nq); n != 2 {
		t.Fatalf("fixture: retention must leave exactly the 2 recent events, left %d", n)
	}
	runWorker(t, env, "engagement_rollup")

	assertEngagementMatchesLedger(t, env, nq, v, "after retention")
	if i, c, _ := storedEngagement(t, env, nq, v); i != 1 || c != 1 {
		t.Errorf("after retention: impressions=%d clicks=%d, want 1/1 — one surviving impression and one "+
			"surviving click", i, c)
	}
}

// TestIntegrationEngagementRetractsAPurgedUsersClicks is the same property from
// the deletion side: #37's clear-all removes the user's behavior_events, and the
// engagement counters must lose their contribution on the next pass — not only
// the ledger.
func TestIntegrationEngagementRetractsAPurgedUsersClicks(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	now := time.Now()
	v := uuid.New()
	const nq = "engagement purge demo"

	clearer, other := uuid.New(), uuid.New()
	ingest(t, env,
		clicked(now, nq, v, &clearer, "ep-1"),
		clicked(now.Add(time.Second), nq, v, &clearer, "ep-1"),
		clicked(now.Add(2*time.Second), nq, v, &other, "ep-2"),
	)
	runWorker(t, env, "engagement_rollup")
	if _, c, _ := storedEngagement(t, env, nq, v); c != 3 {
		t.Fatalf("fixture: three clicks must fold to clicks = 3, got %d", c)
	}

	if err := env.history.ClearAll(ctx, clearer); err != nil {
		t.Fatalf("ClearAll: %v", err)
	}
	runWorker(t, env, "engagement_rollup")

	assertEngagementMatchesLedger(t, env, nq, v, "after the clearer's purge")
	if _, c, _ := storedEngagement(t, env, nq, v); c != 1 {
		t.Errorf("after clear-all: clicks = %d, want 1 — only the other user's click survives. A cumulative "+
			"counter is a place the clearer's contribution outlives their deletion", c)
	}
}

// TestIntegrationReevaluationIsIdempotentOverTheCounters pins the convergence
// property the daily pass's bounded loop depends on: a row it has fixed does not
// come back. That loop has no cursor — it relies on every batch selecting ONLY
// rows whose values actually move, so the remaining work strictly shrinks. Adding
// the popularity counters to that predicate is where it could have been lost, and
// it is why the predicate is exact integers only: a float compared for equality
// every pass is how a repair loop starts churning on rounding for ever.
func TestIntegrationReevaluationIsIdempotentOverTheCounters(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	now := time.Now()

	// A spread of shapes: above the floor, below it, and one with rows at several
	// timestamps so decayed_freq is not a whole number.
	searchedByAt(t, env, now, "idem plenty", 5)
	searchedByAt(t, env, now, "idem sparse", 1)
	clearer := uuid.New()
	for i := 0; i < 4; i++ {
		u := uuid.New()
		ingest(t, env, submitted(now.Add(-time.Duration(i)*17*time.Hour), "idem spread", &u, "", false))
	}
	ingest(t, env,
		submitted(now, "idem plenty", &clearer, "idem-c", true),
		submitted(now.Add(time.Second), "idem spread", &clearer, "idem-c", true))
	runWorker(t, env, "aggregates_rollup")

	// Give the first pass real work: the clearer's rows go and no further traffic
	// arrives, so only the daily pass can notice. Without this the test would be
	// vacuous — two passes that both change nothing prove nothing about
	// convergence.
	if err := env.history.ClearAll(ctx, clearer); err != nil {
		t.Fatalf("ClearAll: %v", err)
	}

	first, err := env.worker.ReevaluateSuggestible(ctx, false)
	if err != nil {
		t.Fatalf("first re-evaluation: %v", err)
	}
	if first.Changed == 0 {
		t.Fatalf("fixture: the first re-evaluation must have work to do after the clear, moved 0 rows")
	}
	second, err := env.worker.ReevaluateSuggestible(ctx, false)
	if err != nil {
		t.Fatalf("second re-evaluation: %v", err)
	}
	if second.Changed != 0 {
		t.Errorf("a second re-evaluation over an unchanged ledger moved %d row(s) (the first moved %d). "+
			"The pass must converge: its batch loop is bounded only by the fact that a row it fixes cannot "+
			"be selected again", second.Changed, first.Changed)
	}
	for _, nq := range []string{"idem plenty", "idem sparse", "idem spread"} {
		assertAggregateMatchesLedger(t, env, nq, "after two re-evaluation passes")
	}
}
