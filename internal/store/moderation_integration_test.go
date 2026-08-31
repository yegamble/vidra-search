//go:build integration

// Integration tests for the suggestion-ban surface: banning a query removes it
// from the global suggestion stream, and — the assertion that matters — the ban
// SURVIVES an aggregates_rollup pass that reprocesses the query with fresh
// traffic. rollups.sql carries the ban forward (`banned = EXCLUDED.banned`) and
// folds it into the recomputed suggestible flag; this test pins that behaviour
// against future edits to the rollup.
package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vidra/vidra-search/internal/event"
	"github.com/vidra/vidra-search/internal/suggest"
)

// bannableQuery is long enough that its prefixes used here exceed the suggestion
// cache's 3-rune ceiling, so the assertions read Postgres and not a cached list.
const bannableQuery = "buy cheap followers"
const bannablePrefix = "buy ch"

// searchedBy submits the query once per distinct user, enough to clear the
// default 3-distinct-user suggestibility threshold.
func searchedBy(t *testing.T, env *testEnv, query string, users int) {
	t.Helper()
	var batch []event.Envelope
	now := time.Now()
	for i := 0; i < users; i++ {
		u := uuid.New()
		batch = append(batch, submitted(now, query, &u, "", false))
	}
	ingest(t, env, batch...)
}

// banState reads the two moderation columns for a normalized query.
func banState(t *testing.T, env *testEnv, nq string) (banned, suggestible bool, exists bool) {
	t.Helper()
	rows, err := env.store.Pool.Query(context.Background(),
		`SELECT banned, suggestible FROM search.query_aggregates WHERE normalized_query = $1`, nq)
	if err != nil {
		t.Fatalf("ban state query: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return false, false, false
	}
	if err := rows.Scan(&banned, &suggestible); err != nil {
		t.Fatalf("scan ban state: %v", err)
	}
	return banned, suggestible, true
}

func suggested(t *testing.T, env *testEnv, prefix, want string) bool {
	t.Helper()
	resp := env.sugg.Suggest(context.Background(), suggest.Request{Query: prefix, Limit: 20})
	return containsStr(suggestionTexts(resp.Suggestions), want)
}

// TestIntegrationSuggestionBanSurvivesRollup is the core regression test for the
// moderation write path.
func TestIntegrationSuggestionBanSurvivesRollup(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	// --- the query earns instance-wide suggestibility honestly ---
	searchedBy(t, env, bannableQuery, 3)
	runWorker(t, env, "aggregates_rollup")

	if banned, suggestible, ok := banState(t, env, bannableQuery); !ok || banned || !suggestible {
		t.Fatalf("precondition: want an unbanned suggestible row, got banned=%v suggestible=%v exists=%v", banned, suggestible, ok)
	}
	if !suggested(t, env, bannablePrefix, bannableQuery) {
		t.Fatalf("precondition: the query must be in autosuggest before it is banned")
	}

	// --- ban it, through the service, using the DISPLAY form ---
	// The service normalizes, so an operator banning "Buy Cheap Followers" hits
	// the same aggregate row the rollup writes.
	resp, err := env.moderation.Ban(ctx, "Buy Cheap Followers")
	if err != nil {
		t.Fatalf("ban: %v", err)
	}
	if resp.NormalizedQuery != bannableQuery {
		t.Fatalf("ban normalized to %q, want %q", resp.NormalizedQuery, bannableQuery)
	}

	// Both columns move: `banned` is what survives a rollup, `suggestible=false`
	// is what makes the ban effective NOW. Suggestibility never decays — a query
	// with no new traffic is never re-evaluated by the rollup (it INNER JOINs the
	// new-traffic batch) and query_aggregates is never pruned — so a ban that set
	// only `banned` would depend on a rollup pass that may never come.
	banned, suggestible, _ := banState(t, env, bannableQuery)
	if !banned || suggestible {
		t.Errorf("after ban: banned=%v suggestible=%v, want true/false", banned, suggestible)
	}
	if suggested(t, env, bannablePrefix, bannableQuery) {
		t.Errorf("a banned query must disappear from autosuggest immediately")
	}

	// --- THE assertion: fresh traffic + a rollup pass must not resurrect it ---
	countBefore := countRows(t, env, "SELECT total_count FROM search.query_aggregates WHERE normalized_query = '"+bannableQuery+"'")
	searchedBy(t, env, bannableQuery, 3)
	runWorker(t, env, "aggregates_rollup")

	countAfter := countRows(t, env, "SELECT total_count FROM search.query_aggregates WHERE normalized_query = '"+bannableQuery+"'")
	if countAfter <= countBefore {
		t.Fatalf("the rollup did not reprocess the banned row (total_count %d -> %d) — the survival assertion below would be vacuous", countBefore, countAfter)
	}
	banned, suggestible, _ = banState(t, env, bannableQuery)
	if !banned {
		t.Errorf("the ban must survive an aggregates_rollup pass (rollups.sql: banned = EXCLUDED.banned)")
	}
	if suggestible {
		t.Errorf("a banned query must not be recomputed as suggestible by the rollup")
	}
	if suggested(t, env, bannablePrefix, bannableQuery) {
		t.Errorf("a banned query must stay out of autosuggest after a rollup")
	}

	// --- it appears on the reviewable ban list ---
	list, err := env.moderation.List(ctx, 0, 0)
	if err != nil {
		t.Fatalf("list bans: %v", err)
	}
	var found bool
	for _, e := range list.Entries {
		if e.NormalizedQuery == bannableQuery {
			found = true
			if e.DistinctUsers < 3 {
				t.Errorf("ban list entry should carry the counts a reviewer needs, got %+v", e)
			}
		}
	}
	if !found {
		t.Fatalf("banned query missing from the ban list: %+v", list.Entries)
	}

	// --- unban does NOT hand back suggestibility directly ---
	if err := env.moderation.Unban(ctx, bannableQuery); err != nil {
		t.Fatalf("unban: %v", err)
	}
	banned, suggestible, _ = banState(t, env, bannableQuery)
	if banned {
		t.Errorf("unban must clear the banned flag")
	}
	if suggestible {
		t.Errorf("unban must NOT set suggestible directly — the rollup recomputes it from real distinct-user counts")
	}
	if suggested(t, env, bannablePrefix, bannableQuery) {
		t.Errorf("an unbanned query stays out of autosuggest until a rollup re-earns it")
	}

	// --- and the next rollup with traffic restores it honestly ---
	searchedBy(t, env, bannableQuery, 3)
	runWorker(t, env, "aggregates_rollup")
	if _, suggestible, _ = banState(t, env, bannableQuery); !suggestible {
		t.Errorf("after the unban, a rollup over qualifying traffic must restore suggestibility")
	}
	if !suggested(t, env, bannablePrefix, bannableQuery) {
		t.Errorf("the unbanned query should be back in autosuggest after a rollup")
	}
}

// TestIntegrationSuggestionBanPreemptsUnseenQuery proves a ban can be placed on a
// string that has no aggregate row yet, so an operator can pre-empt a known-bad
// phrase instead of waiting for it to reach the suggestion threshold first.
func TestIntegrationSuggestionBanPreemptsUnseenQuery(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	const unseen = "slur placeholder phrase"

	if _, _, ok := banState(t, env, unseen); ok {
		t.Fatalf("precondition: %q must have no aggregate row", unseen)
	}
	if _, err := env.moderation.Ban(ctx, unseen); err != nil {
		t.Fatalf("pre-emptive ban: %v", err)
	}
	banned, suggestible, ok := banState(t, env, unseen)
	if !ok || !banned || suggestible {
		t.Fatalf("pre-emptive ban: banned=%v suggestible=%v exists=%v, want true/false/true", banned, suggestible, ok)
	}

	// Real traffic then arrives and clears the distinct-user threshold; the
	// placeholder must absorb it without ever becoming suggestible.
	searchedBy(t, env, unseen, 5)
	runWorker(t, env, "aggregates_rollup")

	banned, suggestible, _ = banState(t, env, unseen)
	if !banned || suggestible {
		t.Errorf("after traffic + rollup: banned=%v suggestible=%v, want true/false", banned, suggestible)
	}
	if total := countRows(t, env, "SELECT total_count FROM search.query_aggregates WHERE normalized_query = '"+unseen+"'"); total != 5 {
		t.Errorf("placeholder must absorb real counts, total_count = %d, want 5", total)
	}
	if suggested(t, env, "slur p", unseen) {
		t.Errorf("a pre-emptively banned query must never reach autosuggest")
	}
}
