//go:build integration

// Integration tests for the k-anonymity floor's ANONYMOUS half.
//
// The floor that decides whether a normalized query becomes instance-wide
// autosuggest counts distinct user_ids plus, for rows carrying no user_id, a
// fallback identifier. That fallback used to be session_id, which comes from the
// client's X-Vidra-Session header — a client rotating the header minted unlimited
// well-formed identities and cleared the default floor of 3 from one request
// loop. vidra-core now derives `subject_id` server-side for anonymous callers
// (address-keyed, day-scoped, frozen into the outbox payload at enqueue), and
// these tests pin that vidra-search actually counts it.
//
// They also pin the two properties that make the change safe to land on a live
// instance: rows written before the migration (subject_id IS NULL) keep counting
// via session_id, and rollups.sql and reevaluation.sql agree on every fixture —
// the two carry the SAME predicate by construction, and a divergence would make
// `suggestible` flap between the rollup and the daily re-evaluation pass.
package store_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vidra/vidra-search/internal/event"
)

// anonSubmitted builds an ANONYMOUS search.submitted envelope: no user_id, with
// an optional session_id and an optional server-derived subject_id. An empty
// subject is how a pre-subject vidra-core (or an anonymous request whose address
// could not be derived) presents on the wire — the field is simply absent.
func anonSubmitted(occurredAt time.Time, query, session, subject string) event.Envelope {
	p := map[string]any{"query": query, "allow_history": false}
	if session != "" {
		p["session_id"] = session
	}
	if subject != "" {
		p["subject_id"] = subject
	}
	raw, _ := json.Marshal(p)
	return event.Envelope{EventID: uuid.New(), Type: event.TypeSearchSubmitted, OccurredAt: occurredAt, SchemaVersion: 1, Payload: raw}
}

// distinctUsers reads the floor the rollup computed for a query.
func distinctUsers(t *testing.T, env *testEnv, nq string) int64 {
	t.Helper()
	return countRows(t, env, `SELECT distinct_users FROM search.query_aggregates WHERE normalized_query = $1`, nq)
}

// TestIntegrationRotatedSessionsOneSubjectCountOnce is THE security property.
// One anonymous actor rotating X-Vidra-Session across N requests presents N
// session ids but one server-derived subject, so the floor must see one subject
// — not N. Before subject_id was counted this query reached distinct_users = N
// and became instance-wide autosuggest from a single machine.
func TestIntegrationRotatedSessionsOneSubjectCountOnce(t *testing.T) {
	env := newTestEnv(t)
	now := time.Now()
	const nq = "buy cheap engagement"
	const subject = "subject-attacker-one"

	var batch []event.Envelope
	for i := 0; i < 5; i++ {
		batch = append(batch, anonSubmitted(now, nq, "rotated-session-"+string(rune('a'+i)), subject))
	}
	ingest(t, env, batch...)
	runWorker(t, env, "aggregates_rollup")

	if n := distinctUsers(t, env, nq); n != 1 {
		t.Errorf("distinct_users = %d, want 1: five rotated session ids from ONE server-derived subject must collapse to one", n)
	}
	if _, suggestible, _ := banState(t, env, nq); suggestible {
		t.Errorf("a query pushed by a single anonymous subject must not become instance-wide autosuggest")
	}
}

// TestIntegrationDistinctSubjectsClearTheFloor is the other half: the fix must
// not be "anonymous traffic never counts". Anonymous traffic is most of a fresh
// instance's traffic, and refusing to count it would starve autosuggest.
func TestIntegrationDistinctSubjectsClearTheFloor(t *testing.T) {
	env := newTestEnv(t)
	now := time.Now()
	const nq = "sourdough starter"

	var batch []event.Envelope
	for i := 0; i < 3; i++ {
		s := string(rune('a' + i))
		batch = append(batch, anonSubmitted(now, nq, "session-"+s, "subject-"+s))
	}
	ingest(t, env, batch...)
	runWorker(t, env, "aggregates_rollup")

	if n := distinctUsers(t, env, nq); n != 3 {
		t.Errorf("distinct_users = %d, want 3: three DISTINCT anonymous subjects are three people for floor purposes", n)
	}
	if _, suggestible, _ := banState(t, env, nq); !suggestible {
		t.Errorf("three distinct anonymous subjects must clear the default floor of 3")
	}
}

// TestIntegrationAuthenticatedAndAnonymousComposeOnce proves the two halves add
// rather than double-count: an authenticated row carries user_id and no
// subject_id, so it must count exactly once through the user_id term and never
// again through the anonymous term.
func TestIntegrationAuthenticatedAndAnonymousComposeOnce(t *testing.T) {
	env := newTestEnv(t)
	now := time.Now()
	const nq = "kubernetes ingress"

	u1, u2 := uuid.New(), uuid.New()
	ingest(t, env,
		submitted(now, nq, &u1, "auth-session-1", false),
		submitted(now, nq, &u2, "auth-session-2", false),
		anonSubmitted(now, nq, "anon-session", "subject-anon"),
	)
	runWorker(t, env, "aggregates_rollup")

	if n := distinctUsers(t, env, nq); n != 3 {
		t.Errorf("distinct_users = %d, want 3 (two signed-in users + one anonymous subject)", n)
	}
}

// TestIntegrationLegacyNullSubjectRowsKeepTheirFloor is the live-instance
// regression guard. Every query_log row written before this migration has
// subject_id IS NULL. If the floor counted subject_id ALONE, those rows would
// stop counting the moment the column landed and the daily re-evaluation pass
// would mass-un-suggest an instance's entire autosuggest corpus on its next run
// — a silent, self-inflicted outage of the feature. The historical rows keep
// their old session fallback and age out at the retention horizon instead.
func TestIntegrationLegacyNullSubjectRowsKeepTheirFloor(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	now := time.Now()
	const nq = "legacy popular query"

	// Exactly what a pre-subject vidra-core emitted: anonymous, session only.
	var batch []event.Envelope
	for i := 0; i < 3; i++ {
		batch = append(batch, anonSubmitted(now, nq, "legacy-session-"+string(rune('a'+i)), ""))
	}
	ingest(t, env, batch...)
	runWorker(t, env, "aggregates_rollup")

	if n := countRows(t, env,
		`SELECT count(*) FROM search.query_log WHERE normalized_query = $1 AND subject_id IS NULL`, nq); n != 3 {
		t.Fatalf("precondition: the fixture must produce 3 NULL-subject rows, got %d", n)
	}
	if _, suggestible, _ := banState(t, env, nq); !suggestible {
		t.Fatalf("historical anonymous rows must still clear the floor through their session fallback")
	}

	// The re-evaluation pass is where a mass un-suggest would actually land: it
	// re-applies the predicate to EVERY aggregate row, not just rows with new
	// traffic.
	rep, err := env.worker.ReevaluateSuggestible(ctx, false)
	if err != nil {
		t.Fatalf("reevaluate: %v", err)
	}
	if rep.Changed != 0 {
		t.Errorf("the re-evaluation pass moved %d row(s) on a ledger of NULL-subject history — this is the mass un-suggest", rep.Changed)
	}
	if _, suggestible, _ := banState(t, env, nq); !suggestible {
		t.Errorf("a query supported only by pre-migration rows lost suggestibility — a live instance would empty its autosuggest on deploy")
	}
}

// TestIntegrationRollupAndReevaluationAgreeOnSubjects is the anti-flap test.
// reevaluation.sql is deliberately rollups.sql's recount with the INNER JOIN
// removed, so the two can never disagree; if an edit changes one predicate and
// not the other, `suggestible` flaps between the rollup and the daily pass on
// every cycle. The fixture straddles the floor in BOTH directions so a
// divergence has to move a flag and cannot hide:
//
//	rotated  — 5 sessions, 1 subject:  1 by subject, 5 by session  (below/above)
//	distinct — 3 sessions, 3 subjects: 3 either way                (control)
//	legacy   — 3 sessions, no subject: 0 by subject, 3 by fallback (below/above)
func TestIntegrationRollupAndReevaluationAgreeOnSubjects(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	now := time.Now()

	var batch []event.Envelope
	for i := 0; i < 5; i++ {
		batch = append(batch, anonSubmitted(now, "rotated query", "rot-session-"+string(rune('a'+i)), "subject-rot"))
	}
	for i := 0; i < 3; i++ {
		s := string(rune('a' + i))
		batch = append(batch, anonSubmitted(now, "distinct query", "dist-session-"+s, "subject-dist-"+s))
	}
	for i := 0; i < 3; i++ {
		batch = append(batch, anonSubmitted(now, "legacy query", "leg-session-"+string(rune('a'+i)), ""))
	}
	ingest(t, env, batch...)
	runWorker(t, env, "aggregates_rollup")

	want := map[string]struct {
		distinct    int64
		suggestible bool
	}{
		"rotated query":  {1, false},
		"distinct query": {3, true},
		"legacy query":   {3, true},
	}
	for nq, w := range want {
		if n := distinctUsers(t, env, nq); n != w.distinct {
			t.Errorf("rollup %q: distinct_users = %d, want %d", nq, n, w.distinct)
		}
		if _, suggestible, _ := banState(t, env, nq); suggestible != w.suggestible {
			t.Errorf("rollup %q: suggestible = %v, want %v", nq, suggestible, w.suggestible)
		}
	}

	// The dry run reports every row the pass WOULD move. On data the rollup has
	// just settled, a shared predicate moves nothing.
	preview, err := env.worker.ReevaluateSuggestible(ctx, true)
	if err != nil {
		t.Fatalf("preview reevaluate: %v", err)
	}
	if preview.Changed != 0 {
		t.Errorf("re-evaluation would move %d row(s) the rollup just settled (%+v) — rollups.sql and reevaluation.sql no longer share one predicate", preview.Changed, preview.Sample)
	}

	// And applying it really changes nothing, so the flag cannot flap.
	rep, err := env.worker.ReevaluateSuggestible(ctx, false)
	if err != nil {
		t.Fatalf("reevaluate: %v", err)
	}
	if rep.Changed != 0 {
		t.Errorf("re-evaluation moved %d row(s) — suggestible will flap between the two passes every cycle", rep.Changed)
	}
	for nq, w := range want {
		if _, suggestible, _ := banState(t, env, nq); suggestible != w.suggestible {
			t.Errorf("after re-evaluation %q: suggestible = %v, want %v (unchanged from the rollup)", nq, suggestible, w.suggestible)
		}
	}
}
