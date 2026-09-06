//go:build integration

// Integration tests for the ruling that CO-VISITATION SUPPORT COUNTERS RESPECT
// EVENT RETENTION.
//
// vidra-search#34 made the k-anonymity FLOOR a live question every pass: the
// number of distinct subjects behind a pair is recomputed from the retained
// `behavior_events` ledger, so an edge whose people age out disappears. It left
// the other half alone. The SCORES behind that floor were still read off the
// cumulative `co_watch` / `co_search` counters, which the accumulators only ever
// incremented and nothing ever pruned — so an event deleted by retention (or by
// a user's purge) kept contributing its co-occurrence to the shrunk cosine of
// every pair that still cleared the floor, forever, and the published score was
// a number no surviving evidence could reproduce.
//
// These tests pin the three properties that fix means:
//
//  1. the published score is what recomputing from the CURRENTLY RETAINED ledger
//     gives, and nothing else feeds it (a fabricated counter row changes nothing,
//     and no counter row is written at all);
//  2. retention retracts support from the SCORE, not only from the floor — a pair
//     that stays above the floor has its score fall to exactly the recomputed
//     value, and one that falls below it disappears;
//  3. removing a user's rows retracts that user's support from the score on the
//     next rollup.
package store_test

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
)

// ledgerCoVisits recomputes, straight from the retained `behavior_events`, how
// many co-visit INSTANCES an unordered pair has — the number the rollup's own
// pairing produces. The predicate is spelled out here rather than imported from
// the production query on purpose: this is the tests' independent second opinion,
// and a helper that ran the shipped SQL would only prove the query agrees with
// itself. `kind` is "watch" (play_started/meaningful_watch in one session) or
// "search" (result_clicked for one query in one session).
func ledgerCoVisits(t *testing.T, env *testEnv, kind string, a, b uuid.UUID) int64 {
	t.Helper()
	var src, extra string
	switch kind {
	case "watch":
		src = `SELECT id, session_id, video_id, occurred_at, NULL::text AS nq
		         FROM search.behavior_events
		        WHERE type IN ('video.play_started', 'video.meaningful_watch')
		          AND session_id IS NOT NULL AND video_id IS NOT NULL`
	case "search":
		src = `SELECT id, session_id, video_id, occurred_at, normalized_query AS nq
		         FROM search.behavior_events
		        WHERE type = 'search.result_clicked'
		          AND session_id IS NOT NULL AND video_id IS NOT NULL
		          AND normalized_query IS NOT NULL`
		extra = ` AND p.nq = n.nq`
	default:
		t.Fatalf("ledgerCoVisits: unknown kind %q", kind)
	}
	q := `WITH ev AS (` + src + `)
	      SELECT count(*) FROM ev n JOIN ev p
	        ON p.session_id = n.session_id
	       AND p.id < n.id
	       AND p.video_id <> n.video_id
	       AND abs(EXTRACT(EPOCH FROM (n.occurred_at - p.occurred_at))) <= 3600` + extra + `
	     WHERE LEAST(n.video_id, p.video_id) = LEAST($1::uuid, $2::uuid)
	       AND GREATEST(n.video_id, p.video_id) = GREATEST($1::uuid, $2::uuid)`
	return countRows(t, env, q, a, b)
}

// covisWatchPairs is the number of DISTINCT co-watched pairs the retained ledger
// still shows — the ledger-side answer to "how many pairs does this instance
// know about", which used to be `SELECT count(*) FROM search.co_watch`.
func covisWatchPairs(t *testing.T, env *testEnv) int64 {
	t.Helper()
	return countRows(t, env, `WITH ev AS (
	        SELECT id, session_id, video_id, occurred_at FROM search.behavior_events
	         WHERE type IN ('video.play_started', 'video.meaningful_watch')
	           AND session_id IS NOT NULL AND video_id IS NOT NULL)
	      SELECT count(*) FROM (
	        SELECT DISTINCT LEAST(n.video_id, p.video_id), GREATEST(n.video_id, p.video_id)
	          FROM ev n JOIN ev p
	            ON p.session_id = n.session_id AND p.id < n.id AND p.video_id <> n.video_id
	           AND abs(EXTRACT(EPOCH FROM (n.occurred_at - p.occurred_at))) <= 3600) d`)
}

// TestIntegrationCovisScoresComeOnlyFromTheRetainedLedger is the ruling stated as
// a single property: `behavior_events` is the ONLY thing the published index is a
// function of. Two halves, because the counters could fail it either way round —
// the rollup must neither WRITE them (a second store that drifts is a second
// truth waiting to be believed) nor READ them (a stale row must not be able to
// move a live score).
func TestIntegrationCovisScoresComeOnlyFromTheRetainedLedger(t *testing.T) {
	env := newTestEnv(t)
	now := time.Now()

	// The A13 score fixture: three distinct signed-in users each watch A1 then A2,
	// one session each. cooc = 3, totI = totJ = 3, so the blend is
	// 0.7 · (3/sqrt(3·3)) · (3/(3+10)) = 0.7 · 3/13 = 0.16154.
	a1, a2 := uuid.New(), uuid.New()
	for i := 0; i < 3; i++ {
		u := uuid.New()
		sess := fmt.Sprintf("cr-a-%d", i)
		ingest(t, env,
			play(now, a1, "", &u, sess, false),
			play(now.Add(time.Second), a2, "", &u, sess, false))
	}

	runWorker(t, env, "covis_rollup")

	score, ok := neighborEdge(t, env, a1, a2)
	if !ok {
		t.Fatalf("three distinct users co-watched A1→A2; the edge must be published")
	}
	if math.Abs(score-0.16153846) > 1e-6 {
		t.Fatalf("A1→A2 score = %.8f, want 0.16154 (0.7 · 3/13)", score)
	}
	if n := ledgerCoVisits(t, env, "watch", a1, a2); n != 3 {
		t.Fatalf("ledger co-visits(A1,A2) = %d, want 3", n)
	}

	// Half one: the cumulative counters are RETIRED — the rollup writes nothing to
	// them, so there is no second record of co-occurrence outliving the ledger it
	// came from.
	if n := countRows(t, env, `SELECT count(*) FROM search.co_watch`); n != 0 {
		t.Errorf("search.co_watch rows after a rollup = %d, want 0 — the counters are retired, "+
			"a rollup that still writes them leaves a cumulative record retention cannot reach", n)
	}
	if n := countRows(t, env, `SELECT count(*) FROM search.co_search`); n != 0 {
		t.Errorf("search.co_search rows after a rollup = %d, want 0 (same reason)", n)
	}

	// Half two: a counter row that says something else cannot move the score. This
	// is the assertion that fails if any future change re-reads the counters for
	// the pair count or for the normalization mass.
	if _, err := env.store.Pool.Exec(context.Background(),
		`INSERT INTO search.co_watch (video_a, video_b, count, updated_at)
		 VALUES (LEAST($1::uuid,$2::uuid), GREATEST($1::uuid,$2::uuid), 999, now())
		 ON CONFLICT (video_a, video_b) DO UPDATE SET count = 999`, a1, a2); err != nil {
		t.Fatalf("seed a stale counter row: %v", err)
	}
	runWorker(t, env, "covis_rollup")

	after, ok := neighborEdge(t, env, a1, a2)
	if !ok {
		t.Fatalf("A1→A2 must still be published")
	}
	if math.Abs(after-0.16153846) > 1e-6 {
		t.Errorf("A1→A2 score = %.8f after a fabricated co_watch.count of 999, want 0.16154 unchanged — "+
			"the score must be a function of the retained ledger alone", after)
	}
}

// TestIntegrationCovisRetentionRetractsSupportFromScores is SC2 of the ruling and
// the failure #34 left behind. Retention deletes the events; before this change
// the counters kept their contribution, so a pair that survived the floor kept a
// score computed from co-visits the instance had already deleted.
//
// Two pairs, so both outcomes are pinned in one pass:
//
//   - G1→G2, FOUR subjects, one of them past retention. It stays above the floor,
//     and its score must fall from 0.7 · (4/sqrt(4·4)) · (4/14) = 0.20000 to
//     0.7 · (3/sqrt(3·3)) · (3/13) = 0.16154 — the recomputed value, not the
//     0.20000 the unpruned counters would still produce.
//   - H1→H2, THREE subjects (the #34 fixture, score 0.16154), one of them past
//     retention. Two are left, below the floor, and the edge must disappear.
func TestIntegrationCovisRetentionRetractsSupportFromScores(t *testing.T) {
	env := newTestEnv(t)
	now := time.Now()
	old := now.Add(-100 * 24 * time.Hour) // past the 90-day retention default

	g1, g2 := uuid.New(), uuid.New()
	ingest(t, env,
		anonPlay(old, g1, "cr-g-old", "subject-fake-g-old"),
		anonPlay(old.Add(time.Second), g2, "cr-g-old", "subject-fake-g-old"))
	for i := 0; i < 3; i++ {
		at := now.Add(time.Duration(i) * time.Minute)
		sess := fmt.Sprintf("cr-g-%d", i)
		ingest(t, env,
			anonPlay(at, g1, sess, fmt.Sprintf("subject-fake-g-%d", i)),
			anonPlay(at.Add(time.Second), g2, sess, fmt.Sprintf("subject-fake-g-%d", i)))
	}

	h1, h2 := uuid.New(), uuid.New()
	ingest(t, env,
		anonPlay(old, h1, "cr-h-old", "subject-fake-h-old"),
		anonPlay(old.Add(time.Second), h2, "cr-h-old", "subject-fake-h-old"))
	for i := 0; i < 2; i++ {
		at := now.Add(time.Duration(i) * time.Minute)
		sess := fmt.Sprintf("cr-h-%d", i)
		ingest(t, env,
			anonPlay(at, h1, sess, fmt.Sprintf("subject-fake-h-%d", i)),
			anonPlay(at.Add(time.Second), h2, sess, fmt.Sprintf("subject-fake-h-%d", i)))
	}

	runWorker(t, env, "covis_rollup")

	before, ok := neighborEdge(t, env, g1, g2)
	if !ok {
		t.Fatalf("four subjects must publish G1→G2")
	}
	if math.Abs(before-0.2) > 1e-6 {
		t.Fatalf("G1→G2 score = %.8f before retention, want 0.20000 (0.7 · 4/14)", before)
	}
	if _, ok := neighborEdge(t, env, h1, h2); !ok {
		t.Fatalf("three subjects must publish H1→H2 before retention")
	}

	runWorker(t, env, "retention")
	if n := ledgerCoVisits(t, env, "watch", g1, g2); n != 3 {
		t.Fatalf("retained co-visits(G1,G2) = %d, want 3 (the 100-day-old session is gone)", n)
	}

	runWorker(t, env, "covis_rollup")

	after, ok := neighborEdge(t, env, g1, g2)
	if !ok {
		t.Fatalf("G1→G2 still has three subjects behind it and must stay published")
	}
	if math.Abs(after-0.16153846) > 1e-6 {
		t.Errorf("G1→G2 score = %.8f after retention, want 0.16154 — the score recomputed from the "+
			"THREE surviving subjects. 0.20000 means the pruned session is still being counted "+
			"out of a cumulative counter retention cannot reach", after)
	}
	if _, ok := neighborEdge(t, env, h1, h2); ok {
		t.Errorf("H1→H2 has two subjects left, below the floor of three, and must disappear")
	}
}

// TestIntegrationCovisPurgeRetractsAUsersSupport is SC3. The A13 opt-out slice
// (vidra-search#35 / vidra-core#169) leans on a user's "Clear all" to take their
// data out of what this service publishes, so the question is whether their
// co-visitation SUPPORT — not just their name on it — actually leaves.
//
// WHEN THIS TEST WAS WRITTEN THE ANSWER WAS NO, AND IT SAID SO. Clear-all did not
// delete `behavior_events`, it NULLed `user_id`, so the row survived, the floor
// fell back to COALESCE(subject_id, session_id), and the co-visit still counted.
// The test pinned that in two acts: act 1 drove the shipped clear-all and
// asserted the score UNCHANGED, act 2 deleted the rows by hand — "what a
// delete-based purge produces" — and asserted the retraction. Two acts because
// the shipped behaviour and the ruling did not agree.
//
// They agree now. Clear-all deletes (queries/history.sql carries the argument:
// anonymizing was not a milder deletion but an inversion of one, because it moved
// the account's rows onto the client-supplied session fallback and turned one
// person into N subjects). So the hand-written DELETE that was act 2 IS act 1,
// and the test is one act: the SHIPPED clear-all must retract the user's support
// from the SCORE and not merely from the floor count — the four-subject pair
// falls to the three-subject score, and the three-subject pair drops below the
// floor and disappears.
func TestIntegrationCovisPurgeRetractsAUsersSupport(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	now := time.Now()

	// P1→P2: three subjects, one of them the signed-in user who will purge.
	// Q1→Q2: four subjects, the same user among them, in a second session.
	victim := uuid.New()
	p1, p2 := uuid.New(), uuid.New()
	q1, q2 := uuid.New(), uuid.New()

	ingest(t, env,
		play(now, p1, "", &victim, "cr-purge-p", false),
		play(now.Add(time.Second), p2, "", &victim, "cr-purge-p", false),
		play(now.Add(time.Minute), q1, "", &victim, "cr-purge-q", false),
		play(now.Add(time.Minute+time.Second), q2, "", &victim, "cr-purge-q", false))
	for i := 0; i < 2; i++ {
		at := now.Add(time.Duration(i+2) * time.Minute)
		ingest(t, env,
			anonPlay(at, p1, fmt.Sprintf("cr-p-%d", i), fmt.Sprintf("subject-fake-p-%d", i)),
			anonPlay(at.Add(time.Second), p2, fmt.Sprintf("cr-p-%d", i), fmt.Sprintf("subject-fake-p-%d", i)))
	}
	for i := 0; i < 3; i++ {
		at := now.Add(time.Duration(i+5) * time.Minute)
		ingest(t, env,
			anonPlay(at, q1, fmt.Sprintf("cr-q-%d", i), fmt.Sprintf("subject-fake-q-%d", i)),
			anonPlay(at.Add(time.Second), q2, fmt.Sprintf("cr-q-%d", i), fmt.Sprintf("subject-fake-q-%d", i)))
	}

	runWorker(t, env, "covis_rollup")
	if s, ok := neighborEdge(t, env, p1, p2); !ok || math.Abs(s-0.16153846) > 1e-6 {
		t.Fatalf("P1→P2 = %.8f (published %v), want 0.16154 with three subjects", s, ok)
	}
	if s, ok := neighborEdge(t, env, q1, q2); !ok || math.Abs(s-0.2) > 1e-6 {
		t.Fatalf("Q1→Q2 = %.8f (published %v), want 0.20000 with four subjects", s, ok)
	}

	// The shipped clear-all. It deletes; there is nothing left to orphan.
	if err := env.history.ClearAll(ctx, victim); err != nil {
		t.Fatalf("ClearAll: %v", err)
	}
	if n := countRows(t, env,
		`SELECT count(*) FROM search.behavior_events WHERE user_id = $1`, victim); n != 0 {
		t.Fatalf("clear-all must leave no row attributed to the user, found %d", n)
	}
	if n := countRows(t, env,
		`SELECT count(*) FROM search.behavior_events WHERE session_id IN ('cr-purge-p','cr-purge-q')`); n != 0 {
		t.Fatalf("clear-all must DELETE the user's rows, not orphan them: %d left in their "+
			"sessions. An orphaned row keeps counting — under its session id, as a NEW subject", n)
	}
	if n := ledgerCoVisits(t, env, "watch", q1, q2); n != 3 {
		t.Fatalf("retained co-visits(Q1,Q2) = %d, want 3 after the clear", n)
	}

	runWorker(t, env, "covis_rollup")

	if _, ok := neighborEdge(t, env, p1, p2); ok {
		t.Errorf("P1→P2 had three subjects and one of them cleared their history; two are left, " +
			"below the floor, so the edge must disappear")
	}
	after, ok := neighborEdge(t, env, q1, q2)
	if !ok {
		t.Fatalf("Q1→Q2 still has three subjects and must stay published")
	}
	if math.Abs(after-0.16153846) > 1e-6 {
		t.Errorf("Q1→Q2 score = %.8f after the clear, want 0.16154 — the cleared user's co-visit "+
			"must leave the SCORE as well as the floor count. 0.20000 means their contribution "+
			"survives somewhere their deletion cannot reach", after)
	}
}
