//go:build integration

// Integration tests for "Clear all" — DELETE /internal/v1/users/{id}/search-history
// (history.ClearAll), the handler behind the frontend's clear-history button.
//
// THE DEFECT THESE PIN. The shipped clear-all did not delete the caller's rows;
// it UPDATEd `user_id` to NULL. Every k-anonymity floor in this service counts
//
//	count(DISTINCT user_id)
//	  + count(DISTINCT CASE WHEN user_id IS NULL THEN COALESCE(subject_id, session_id) END)
//
// and an ATTRIBUTED row carries no subject_id — core mints the day-scoped
// anonymous pseudonym only for callers with no account, because beside a known
// account id it would be redundant and a leak. So NULLing user_id dropped every
// one of that account's rows through to the session fallback, and one person who
// had used three sessions stopped being one subject and became THREE. The floors
// that are recomputed from the ledger — autosuggest's `distinct_users` and the
// co-visitation neighbour gate — then read the clearer as a crowd:
//
//	subjects(V1,V2) = 1 before "Clear all"   →   3 after
//
// which is not a privacy action failing to help, it is a privacy action that
// PUBLISHES. A pair suppressed as one person's browsing appears in the global
// "watch this next" index, and a query private enough to be below the floor
// becomes an instance-wide autosuggest completion. Any single account could do it
// deliberately, on demand, by pressing the button offered to it for privacy.
//
// The owner's standing ruling for this area is that opt-out means no attributed
// collection and that a user's contribution must leave the floor AND the scores
// when their data goes. Clear-all therefore DELETES. Deletion is also what makes
// the bypass structurally impossible rather than merely fixed: removing rows can
// only ever lower a distinct-subject count, never raise one.
package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vidra/vidra-search/internal/event"
)

// ledgerCoVisitSubjects is the tests' independent second opinion on how many
// distinct subjects the RETAINED ledger says co-visited a pair. Like
// ledgerCoVisits (covis_retention_integration_test.go) it spells the predicate
// out here rather than calling the shipped query, so it can disagree with the
// production SQL instead of only proving that SQL agrees with itself.
func ledgerCoVisitSubjects(t *testing.T, env *testEnv, a, b uuid.UUID) int64 {
	t.Helper()
	return countRows(t, env, `WITH ev AS (
	        SELECT id, session_id, video_id, occurred_at, user_id, props->>'subject_id' AS subject_id
	          FROM search.behavior_events
	         WHERE type IN ('video.play_started', 'video.meaningful_watch')
	           AND session_id IS NOT NULL AND video_id IS NOT NULL)
	      SELECT (count(DISTINCT n.user_id)
	            + count(DISTINCT CASE WHEN n.user_id IS NULL
	                                  THEN COALESCE(n.subject_id, n.session_id) END))::bigint
	        FROM ev n JOIN ev p
	          ON p.session_id = n.session_id AND p.id < n.id AND p.video_id <> n.video_id
	         AND abs(EXTRACT(EPOCH FROM (n.occurred_at - p.occurred_at))) <= 3600
	       WHERE LEAST(n.video_id, p.video_id) = LEAST($1::uuid, $2::uuid)
	         AND GREATEST(n.video_id, p.video_id) = GREATEST($1::uuid, $2::uuid)`, a, b)
}

// aggregateRow reads the two fields of search.query_aggregates that the
// distinct-user floor writes: the recomputed subject count and the flag it gates.
func aggregateRow(t *testing.T, env *testEnv, nq string) (int32, bool) {
	t.Helper()
	var distinctUsers int32
	var suggestible bool
	if err := env.store.Pool.QueryRow(context.Background(),
		`SELECT distinct_users, suggestible FROM search.query_aggregates WHERE normalized_query = $1`,
		nq).Scan(&distinctUsers, &suggestible); err != nil {
		t.Fatalf("read query_aggregates[%q]: %v", nq, err)
	}
	return distinctUsers, suggestible
}

// redisKeyExists reports whether a key is present, so a test can assert on Redis
// state the same way it asserts on rows.
func redisKeyExists(t *testing.T, env *testEnv, key string) bool {
	t.Helper()
	n, err := env.cache.Client.Exists(context.Background(), key).Result()
	if err != nil {
		t.Fatalf("EXISTS %s: %v", key, err)
	}
	return n > 0
}

// TestIntegrationClearAllCannotPublishTheClearersCoVisits is the bypass, stated
// as the smallest fixture that shows it: ONE signed-in person, ONE pair, THREE
// sessions. The pair is correctly suppressed — one subject, below a floor of
// three — and pressing "Clear all" must not be the way to get it published.
func TestIntegrationClearAllCannotPublishTheClearersCoVisits(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	now := time.Now()

	clearer := uuid.New()
	v1, v2 := uuid.New(), uuid.New()
	for i, sess := range []string{"hc-cov-1", "hc-cov-2", "hc-cov-3"} {
		at := now.Add(time.Duration(i) * time.Minute)
		ingest(t, env,
			play(at, v1, "", &clearer, sess, false),
			play(at.Add(time.Second), v2, "", &clearer, sess, false))
	}

	if n := ledgerCoVisitSubjects(t, env, v1, v2); n != 1 {
		t.Fatalf("before clear-all: subjects(V1,V2) = %d, want 1 — one account in three sessions "+
			"is one person, counted through the user_id term", n)
	}
	runWorker(t, env, "covis_rollup")
	if s, ok := neighborEdge(t, env, v1, v2); ok {
		t.Fatalf("V1→V2 is one person's browsing and must stay below the floor of three, "+
			"but it is published at %.8f", s)
	}

	if err := env.history.ClearAll(ctx, clearer); err != nil {
		t.Fatalf("ClearAll: %v", err)
	}

	if n := ledgerCoVisitSubjects(t, env, v1, v2); n != 0 {
		t.Errorf("after clear-all: subjects(V1,V2) = %d, want 0. Any number ABOVE the 1 it was "+
			"before means the clear PUBLISHED an association that was correctly suppressed: an "+
			"attributed row carries no subject_id, so NULLing user_id drops each of the account's "+
			"sessions through COALESCE(subject_id, session_id) and one person becomes three "+
			"'subjects'. Clear-all must DELETE the rows", n)
	}
	if n := ledgerCoVisits(t, env, "watch", v1, v2); n != 0 {
		t.Errorf("after clear-all: co-visits(V1,V2) = %d, want 0 — the support must leave the "+
			"score, not only the floor count", n)
	}
	runWorker(t, env, "covis_rollup")
	if s, ok := neighborEdge(t, env, v1, v2); ok {
		t.Errorf("V1→V2 published at %.8f AFTER its only viewer cleared their history. Pressing "+
			"\"Clear all\" must never be a way to publish your own browsing to every viewer", s)
	}
}

// TestIntegrationClearAllCannotPromoteAQueryIntoAutosuggest is the same bypass on
// the surface it was designed for. `minimum_query_user_count` exists so a rare,
// personal query never becomes a globally-suggested phrase; a query one account
// typed in three sessions is one person and must stay below it, before and after
// that account clears its history.
func TestIntegrationClearAllCannotPromoteAQueryIntoAutosuggest(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	now := time.Now()

	clearer := uuid.New()
	const nq = "hc private thing"

	for i, sess := range []string{"hc-agg-1", "hc-agg-2", "hc-agg-3"} {
		ingest(t, env, submitted(now.Add(time.Duration(i)*time.Minute), nq, &clearer, sess, true))
	}
	// Unrelated traffic from someone else, so the re-evaluation pass's
	// non-empty-window guard still holds once the clearer's rows are gone (an
	// empty window makes that pass refuse to run, which would hide the result).
	ingest(t, env, anonSubmitted(now, "hc unrelated", "hc-other", "subject-fake-other"))

	runWorker(t, env, "aggregates_rollup")
	if du, sug := aggregateRow(t, env, nq); du != 1 || sug {
		t.Fatalf("before clear-all: distinct_users = %d suggestible = %v, want 1 / false", du, sug)
	}

	if err := env.history.ClearAll(ctx, clearer); err != nil {
		t.Fatalf("ClearAll: %v", err)
	}

	// The daily re-evaluation pass recounts EVERY aggregate row from the
	// surviving query_log — the shipped path by which a deletion reaches the
	// floor without waiting for new traffic on that string.
	runWorker(t, env, "suggestible_reeval")
	du, sug := aggregateRow(t, env, nq)
	if du > 1 {
		t.Errorf("after clear-all: distinct_users = %d, was 1 before. A privacy action must never "+
			"RAISE a distinct-subject count — the account's three sessions were counted as three "+
			"anonymous subjects because the rows survived with user_id NULL", du)
	}
	if sug {
		t.Errorf("after clear-all: suggestible = true. One account's private query has been " +
			"promoted into instance-wide autosuggest by the button it pressed for privacy")
	}

	// …and the rollup path, which recounts a string only when it carries new
	// traffic. One genuine anonymous search must leave exactly one subject behind.
	ingest(t, env, anonSubmitted(now.Add(time.Hour), nq, "hc-agg-new", "subject-fake-new"))
	runWorker(t, env, "aggregates_rollup")
	du, sug = aggregateRow(t, env, nq)
	if du != 1 {
		t.Errorf("after clear-all + one genuine anonymous search: distinct_users = %d, want 1 — "+
			"only the surviving searcher counts", du)
	}
	if sug {
		t.Errorf("after clear-all + one genuine anonymous search: suggestible = true, want false")
	}
}

// TestIntegrationClearAllDeletesEveryRowThatNamesTheAccount is the SC2 sweep: no
// row in any per-user table still references the account, by column OR inside the
// raw `props` payload, and the rows are gone rather than orphaned. It also pins
// the two things clear-all must NOT do — resurrect on redelivery, and take the
// watch projection with it (that is the `all` scope, driven by core).
func TestIntegrationClearAllDeletesEveryRowThatNamesTheAccount(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	now := time.Now()

	clearer := uuid.New()
	v := uuid.New()
	const sess = "hc-sweep-1"

	// A spread wide enough that every writer of an attributed row is represented:
	// query_log + behavior_events (search.submitted), the personal projections
	// (allow_history), a click, and a watch_progress the engagement worker turns
	// into a DERIVED video.meaningful_watch that copies the account id.
	evs := []event.Envelope{
		submitted(now, "hc sweep query", &clearer, sess, true),
		play(now.Add(time.Second), v, "hc sweep query", &clearer, sess, true),
		clicked(now.Add(2*time.Second), "hc sweep query", v, &clearer, sess),
		watch(now.Add(3*time.Second), v, 600, &clearer, sess, true),
	}
	ingest(t, env, evs...)
	// Give the workers a pass so the DERIVED rows (video.meaningful_watch,
	// sessionizer output) that carry the account id also exist before the clear.
	runWorker(t, env, "engagement_rollup")
	runWorker(t, env, "sessionizer")

	if n := countRows(t, env,
		`SELECT count(*) FROM search.behavior_events WHERE user_id = $1`, clearer); n < 4 {
		t.Fatalf("fixture too thin: %d attributed behavior_events, want at least 4", n)
	}
	if !redisKeyExists(t, env, "sess:q:"+sess) || !redisKeyExists(t, env, "sess:v:"+sess) {
		t.Fatalf("fixture: the session recency lists must exist before the clear")
	}
	inboxBefore := countRows(t, env, `SELECT count(*) FROM search.events_inbox`)

	if err := env.history.ClearAll(ctx, clearer); err != nil {
		t.Fatalf("ClearAll: %v", err)
	}

	// (a) Nothing names the account any more — column or props.
	if n := countRows(t, env,
		`SELECT count(*) FROM search.query_log WHERE user_id = $1`, clearer); n != 0 {
		t.Errorf("query_log still references the account in %d rows", n)
	}
	if n := countRows(t, env,
		`SELECT count(*) FROM search.behavior_events
		  WHERE user_id = $1 OR props->>'user_id' = $1::text`, clearer); n != 0 {
		t.Errorf("behavior_events still references the account in %d rows (column or props)", n)
	}
	if n := countRows(t, env,
		`SELECT count(*) FROM search.user_search_history WHERE user_id = $1`, clearer); n != 0 {
		t.Errorf("user_search_history still holds %d rows for the account", n)
	}

	// (b) The rows are GONE, not orphaned. An orphaned row is exactly the bypass:
	// it keeps its session_id and starts counting as an anonymous subject.
	if n := countRows(t, env,
		`SELECT count(*) FROM search.behavior_events WHERE session_id = $1`, sess); n != 0 {
		t.Errorf("clear-all left %d orphaned behavior_events in the account's session — orphaned "+
			"rows are what the k-floor miscounts as extra people", n)
	}
	if n := countRows(t, env,
		`SELECT count(*) FROM search.query_log WHERE session_id = $1`, sess); n != 0 {
		t.Errorf("clear-all left %d orphaned query_log rows in the account's session", n)
	}

	// (c) Redis: the 2h session recency lists that autosuggest reads back are the
	// one place a cleared query could still be served to the person who cleared it.
	if redisKeyExists(t, env, "sess:q:"+sess) {
		t.Errorf("sess:q:%s survives clear-all — autosuggest would serve the cleared queries "+
			"back to the account for up to the session TTL", sess)
	}
	if redisKeyExists(t, env, "sess:v:"+sess) {
		t.Errorf("sess:v:%s survives clear-all", sess)
	}

	// (d) The dedupe ledger is DELIBERATELY kept: it is the tombstone that stops
	// an at-least-once redelivery from resurrecting a deleted row. It carries no
	// account reference of its own (event_id, type, received_at).
	if n := countRows(t, env, `SELECT count(*) FROM search.events_inbox`); n != inboxBefore {
		t.Errorf("events_inbox went %d → %d; the dedupe ledger must survive a clear so a "+
			"redelivered event cannot bring the deleted rows back", inboxBefore, n)
	}
	res := ingest(t, env, evs...)
	if res.Accepted != 0 {
		t.Errorf("redelivering the cleared events accepted %d of them; every one must be a "+
			"duplicate (%d duplicates seen)", res.Accepted, res.Duplicates)
	}
	if n := countRows(t, env,
		`SELECT count(*) FROM search.behavior_events WHERE session_id = $1`, sess); n != 0 {
		t.Errorf("a redelivery resurrected %d cleared rows", n)
	}

	// (f) The Redis state that is NOT removed, and why that is still a bounded
	// answer rather than a leak with no end. The per-subject trending guard keys
	// the account id INTO the key name, but it is a rate-limit token, and finding
	// them means a MATCH sweep of the whole keyspace on every clear; the
	// distinct-subject HyperLogLog holds the account id as a sketched element,
	// which cannot be removed from an HLL at all. Both expire — the guard with
	// SEARCH_TREND_CAP_WINDOW, the HLL with the 8-day per-day counter TTL — and
	// this pins that "expires" is a fact about the keys and not a hope.
	for _, k := range boundedAccountKeys(t, env, clearer, v) {
		ttl, err := env.cache.Client.TTL(ctx, k).Result()
		if err != nil {
			t.Fatalf("TTL %s: %v", k, err)
		}
		if ttl <= 0 {
			t.Errorf("%s has TTL %v — account-keyed Redis state that clear-all does not delete "+
				"must at least expire on a stated bound", k, ttl)
		}
	}

	// (e) The watch projection is NOT search scope. Clearing search history must
	// not silently delete watch personalization; that is the `all` scope, and core
	// decides which one the user asked for.
	if n := countRows(t, env,
		`SELECT count(*) FROM search.user_watch_projection WHERE user_id = $1`, clearer); n != 1 {
		t.Errorf("user_watch_projection rows for the account = %d, want 1 kept (search scope)", n)
	}
}

// TestIntegrationDeleteEntryRemovesThatQuerysLedgerRows is SC2's consistency
// half. "Forget I searched X" and "forget everything" must mean the same kind of
// thing: if clear-all deletes, the per-entry delete cannot leave the account's
// rows for that one query sitting in the ledger, still counted toward X's
// instance-wide distinct-subject floor.
func TestIntegrationDeleteEntryRemovesThatQuerysLedgerRows(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	now := time.Now()

	u := uuid.New()
	const gone, kept = "hc forget me", "hc keep me"
	v := uuid.New()
	ingest(t, env,
		submitted(now, gone, &u, "hc-entry-1", true),
		submitted(now.Add(time.Second), gone, &u, "hc-entry-2", true),
		clicked(now.Add(2*time.Second), gone, v, &u, "hc-entry-1"),
		submitted(now.Add(3*time.Second), kept, &u, "hc-entry-1", true))

	if err := env.history.DeleteEntry(ctx, u, gone); err != nil {
		t.Fatalf("DeleteEntry: %v", err)
	}

	if n := countRows(t, env,
		`SELECT count(*) FROM search.user_search_history WHERE user_id = $1 AND normalized_query = $2`,
		u, gone); n != 0 {
		t.Errorf("the history entry itself survives (%d rows)", n)
	}
	if n := countRows(t, env,
		`SELECT count(*) FROM search.query_log WHERE user_id = $1 AND normalized_query = $2`,
		u, gone); n != 0 {
		t.Errorf("%d query_log rows for the deleted entry survive, still counted toward that "+
			"query's instance-wide distinct-subject floor", n)
	}
	if n := countRows(t, env,
		`SELECT count(*) FROM search.behavior_events WHERE user_id = $1 AND normalized_query = $2`,
		u, gone); n != 0 {
		t.Errorf("%d behavior_events for the deleted entry survive", n)
	}

	// Nothing else moves: the other query, and the same query for anyone else.
	if n := countRows(t, env,
		`SELECT count(*) FROM search.query_log WHERE user_id = $1 AND normalized_query = $2`,
		u, kept); n != 1 {
		t.Errorf("the untouched query lost rows: %d, want 1", n)
	}
	if n := countRows(t, env,
		`SELECT count(*) FROM search.user_search_history WHERE user_id = $1`, u); n != 1 {
		t.Errorf("history rows for the account = %d, want 1 (only the deleted entry goes)", n)
	}
}

// TestIntegrationClearAllLeavesTheAnonymousPathAlone is SC4. Deleting one
// account's rows must be exactly that: the legitimate anonymous identities keep
// counting one subject per day-scoped pseudonym, a legacy row carrying neither
// user_id nor subject_id still falls back to its session, and another account is
// untouched.
func TestIntegrationClearAllLeavesTheAnonymousPathAlone(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	now := time.Now()

	clearer, bystander := uuid.New(), uuid.New()
	v1, v2 := uuid.New(), uuid.New()

	// The clearer, in one session.
	ingest(t, env,
		play(now, v1, "", &clearer, "hc-anon-me", false),
		play(now.Add(time.Second), v2, "", &clearer, "hc-anon-me", false))
	// Another signed-in account.
	ingest(t, env,
		play(now.Add(time.Minute), v1, "", &bystander, "hc-anon-you", false),
		play(now.Add(time.Minute+time.Second), v2, "", &bystander, "hc-anon-you", false))
	// A real anonymous visitor: NULL user_id WITH core's day-scoped pseudonym.
	// Two sessions, one pseudonym — that is one subject, not two.
	for i, sess := range []string{"hc-anon-a1", "hc-anon-a2"} {
		at := now.Add(time.Duration(i+2) * time.Minute)
		ingest(t, env,
			anonPlay(at, v1, sess, "subject-fake-anon"),
			anonPlay(at.Add(time.Second), v2, sess, "subject-fake-anon"))
	}
	// A legacy row: neither user_id nor subject_id, counted through session_id.
	ingest(t, env,
		anonPlay(now.Add(9*time.Minute), v1, "hc-anon-legacy", ""),
		anonPlay(now.Add(9*time.Minute+time.Second), v2, "hc-anon-legacy", ""))

	if n := ledgerCoVisitSubjects(t, env, v1, v2); n != 4 {
		t.Fatalf("before clear-all: subjects(V1,V2) = %d, want 4 (two accounts + one pseudonym "+
			"across two sessions + one legacy session)", n)
	}

	if err := env.history.ClearAll(ctx, clearer); err != nil {
		t.Fatalf("ClearAll: %v", err)
	}

	if n := ledgerCoVisitSubjects(t, env, v1, v2); n != 3 {
		t.Errorf("after clear-all: subjects(V1,V2) = %d, want exactly 3 — the clearer leaves and "+
			"NOBODY else moves. 4 means the clearer is still counted, 5+ means clearing minted "+
			"identities, below 3 means it took someone else's evidence with it", n)
	}
	if n := countRows(t, env,
		`SELECT count(*) FROM search.behavior_events WHERE user_id = $1`, bystander); n != 2 {
		t.Errorf("the other account's rows = %d, want 2 untouched", n)
	}
	if n := countRows(t, env,
		`SELECT count(*) FROM search.behavior_events WHERE props->>'subject_id' = 'subject-fake-anon'`); n != 4 {
		t.Errorf("the anonymous pseudonym's rows = %d, want 4 untouched", n)
	}
	if n := countRows(t, env,
		`SELECT count(*) FROM search.behavior_events WHERE session_id = 'hc-anon-legacy'`); n != 2 {
		t.Errorf("the legacy (no subject) rows = %d, want 2 untouched", n)
	}
}

// boundedAccountKeys returns the Redis keys that carry the account id but are not
// deleted by a clear: the per-(subject,item) trending ranking guard, and the
// per-day distinct-subject HyperLogLog the account was PFADDed into.
func boundedAccountKeys(t *testing.T, env *testEnv, user, video uuid.UUID) []string {
	t.Helper()
	day := time.Now().UTC().Format("20060102")
	keys := []string{
		"guard:trend:v:" + user.String() + ":" + video.String(),
		"hll:v:" + video.String() + ":" + day,
	}
	for _, k := range keys {
		if !redisKeyExists(t, env, k) {
			t.Fatalf("fixture: expected %s to exist so its bound can be asserted", k)
		}
	}
	return keys
}
