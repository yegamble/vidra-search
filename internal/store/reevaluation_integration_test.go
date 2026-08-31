//go:build integration

// Integration tests for the suggestible re-evaluation pass. The rollup recomputes
// `suggestible` ONLY for queries appearing in its new-traffic batch CTE (an INNER
// JOIN), and nothing prunes query_aggregates — so a string that once cleared the
// distinct-user floor stayed suggestible forever, even after retention deleted
// the query_log rows that justified it. These tests pin the pass that closes that
// gap, and — as importantly — pin the safety rails on it: never un-ban, never act
// on an empty ledger, and never change anything in dry-run mode.
package store_test

import (
	"context"
	"testing"
)

// The two fixtures are already lowercase and single-spaced, so each equals its
// own normalized form and can be used directly as a query_aggregates key.
const (
	dormantQuery = "how to fake engagement"
	livelyQuery  = "how to bake sourdough"
)

// forgetQueryLog simulates what retention (or a purge) does to the evidence
// behind an aggregate row: the query_log rows disappear while query_aggregates —
// which no DELETE in the schema touches — survives untouched.
func forgetQueryLog(t *testing.T, env *testEnv, nq string) {
	t.Helper()
	if _, err := env.store.Pool.Exec(context.Background(),
		`DELETE FROM search.query_log WHERE normalized_query = $1`, nq); err != nil {
		t.Fatalf("forget query_log for %q: %v", nq, err)
	}
}

// seedSuggestible drives a query over the distinct-user floor and rolls it up, so
// the caller starts from an honestly-earned suggestible row.
func seedSuggestible(t *testing.T, env *testEnv, query string) {
	t.Helper()
	searchedBy(t, env, query, 3)
	runWorker(t, env, "aggregates_rollup")
	if _, suggestible, ok := banState(t, env, query); !ok || !suggestible {
		t.Fatalf("precondition: %q must be suggestible after the rollup (exists=%v suggestible=%v)", query, ok, suggestible)
	}
}

// TestIntegrationReevaluationDropsUnsupportedSuggestions is the core regression
// test: evidence gone -> suggestion gone, evidence present -> suggestion kept.
func TestIntegrationReevaluationDropsUnsupportedSuggestions(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	seedSuggestible(t, env, dormantQuery)
	seedSuggestible(t, env, livelyQuery)

	// Retention deletes the dormant query's evidence. Today nothing re-evaluates
	// it: the rollup only revisits queries with NEW traffic, so it stays
	// suggestible on the strength of rows that no longer exist.
	forgetQueryLog(t, env, dormantQuery)
	runWorker(t, env, "aggregates_rollup")
	if _, suggestible, _ := banState(t, env, dormantQuery); !suggestible {
		t.Fatalf("precondition: the rollup must NOT have re-evaluated the dormant row — the assertion below would be vacuous")
	}

	rep, err := env.worker.ReevaluateSuggestible(ctx, false)
	if err != nil {
		t.Fatalf("reevaluate: %v", err)
	}
	if rep.Skipped {
		t.Fatalf("the pass must not skip: the surviving ledger still holds the lively query's rows")
	}
	if rep.Changed != 1 {
		t.Errorf("changed = %d, want exactly 1 (only the dormant row lost its evidence)", rep.Changed)
	}

	if _, suggestible, _ := banState(t, env, dormantQuery); suggestible {
		t.Errorf("a query with no surviving query_log rows must not stay suggestible")
	}
	if _, suggestible, _ := banState(t, env, livelyQuery); !suggestible {
		t.Errorf("a query whose evidence survives must stay suggestible — the pass must not be a blanket un-suggest")
	}

	// The flipped row's distinct_users must agree with the flag it now carries;
	// leaving the rollup's stale high-water mark behind would make the operator
	// surface read "not suggestible, 3 distinct users" and look like a bug.
	if n := countRows(t, env,
		`SELECT distinct_users FROM search.query_aggregates WHERE normalized_query = $1`, dormantQuery); n != 0 {
		t.Errorf("distinct_users = %d after the flip, want 0 to match the surviving evidence", n)
	}

	// Idempotent: a second pass finds nothing left to do.
	rep2, err := env.worker.ReevaluateSuggestible(ctx, false)
	if err != nil {
		t.Fatalf("second reevaluate: %v", err)
	}
	if rep2.Changed != 0 {
		t.Errorf("second pass changed = %d, want 0 (the pass must converge)", rep2.Changed)
	}
}

// TestIntegrationReevaluationRestoresRecoveredEvidence pins the pass as the
// rollup's predicate applied to every row rather than a one-way ratchet: a row
// whose evidence is present but which carries a stale false flag is corrected.
func TestIntegrationReevaluationRestoresRecoveredEvidence(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	seedSuggestible(t, env, livelyQuery)
	// A ban zeroed the flag; the ban is then lifted. UnbanSuggestion deliberately
	// does not restore suggestibility, and no further traffic ever arrives — so
	// without this pass the query can never return despite clearing the floor.
	if _, err := env.moderation.Ban(ctx, livelyQuery); err != nil {
		t.Fatalf("ban: %v", err)
	}
	if err := env.moderation.Unban(ctx, livelyQuery); err != nil {
		t.Fatalf("unban: %v", err)
	}
	if _, suggestible, _ := banState(t, env, livelyQuery); suggestible {
		t.Fatalf("precondition: an unban must not itself restore suggestibility")
	}

	rep, err := env.worker.ReevaluateSuggestible(ctx, false)
	if err != nil {
		t.Fatalf("reevaluate: %v", err)
	}
	if rep.Changed != 1 {
		t.Errorf("changed = %d, want 1", rep.Changed)
	}
	if _, suggestible, _ := banState(t, env, livelyQuery); !suggestible {
		t.Errorf("an unbanned query still clearing the distinct-user floor must be restored by the pass")
	}
}

// TestIntegrationReevaluationNeverUnbans is the safety assertion that matters
// most: the pass computes suggestibility, it does not adjudicate bans. A banned
// row stays banned and stays non-suggestible no matter how much traffic it has.
func TestIntegrationReevaluationNeverUnbans(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	seedSuggestible(t, env, dormantQuery)
	if _, err := env.moderation.Ban(ctx, dormantQuery); err != nil {
		t.Fatalf("ban: %v", err)
	}
	// Plenty of fresh, honest traffic — exactly the input that would make an
	// unbanned row suggestible.
	searchedBy(t, env, dormantQuery, 5)
	searchedBy(t, env, livelyQuery, 3)

	if _, err := env.worker.ReevaluateSuggestible(ctx, false); err != nil {
		t.Fatalf("reevaluate: %v", err)
	}

	banned, suggestible, ok := banState(t, env, dormantQuery)
	if !ok {
		t.Fatalf("the banned row disappeared")
	}
	if !banned {
		t.Errorf("the pass must never clear `banned` — got banned=false")
	}
	if suggestible {
		t.Errorf("a banned query must stay non-suggestible regardless of traffic — got suggestible=true")
	}
}

// TestIntegrationReevaluationDryRun proves the operator can measure the blast
// radius before taking it. A remediation that silently empties autosuggest is a
// worse outcome than the bug it fixes.
func TestIntegrationReevaluationDryRun(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	seedSuggestible(t, env, dormantQuery)
	seedSuggestible(t, env, livelyQuery)
	forgetQueryLog(t, env, dormantQuery)

	rep, err := env.worker.ReevaluateSuggestible(ctx, true)
	if err != nil {
		t.Fatalf("dry-run reevaluate: %v", err)
	}
	if !rep.DryRun {
		t.Errorf("the report must declare itself a dry run")
	}
	if rep.Changed != 1 {
		t.Errorf("dry-run changed = %d, want 1 — a dry run that reports nothing tells the operator nothing", rep.Changed)
	}
	if len(rep.Sample) != 1 || rep.Sample[0].NormalizedQuery != dormantQuery {
		t.Errorf("dry-run sample = %+v, want the dormant query so the operator can see WHICH rows move", rep.Sample)
	}
	if rep.Sample[0].Suggestible {
		t.Errorf("the sample must report the value the row WOULD take (false), got true")
	}

	// Nothing moved.
	if _, suggestible, _ := banState(t, env, dormantQuery); !suggestible {
		t.Errorf("a dry run must not change any row")
	}
	if _, suggestible, _ := banState(t, env, livelyQuery); !suggestible {
		t.Errorf("a dry run must not change any row")
	}
}

// TestIntegrationReevaluationSkipsEmptyLedger is the reconcile-orphan guard: a
// repair pass that runs on an empty ledger concludes that nothing is supported by
// evidence and un-suggests the entire instance. An empty window is the signature
// of incomplete data, not of universally-unpopular queries, so the pass refuses.
func TestIntegrationReevaluationSkipsEmptyLedger(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	seedSuggestible(t, env, dormantQuery)
	seedSuggestible(t, env, livelyQuery)
	if _, err := env.store.Pool.Exec(ctx, `TRUNCATE search.query_log`); err != nil {
		t.Fatalf("truncate query_log: %v", err)
	}

	rep, err := env.worker.ReevaluateSuggestible(ctx, false)
	if err != nil {
		t.Fatalf("reevaluate: %v", err)
	}
	if !rep.Skipped {
		t.Errorf("the pass must skip an empty ledger, got Skipped=false")
	}
	if rep.Changed != 0 {
		t.Errorf("a skipped pass must change nothing, got changed = %d", rep.Changed)
	}
	for _, nq := range []string{dormantQuery, livelyQuery} {
		if _, suggestible, _ := banState(t, env, nq); !suggestible {
			t.Errorf("%q lost suggestibility to a pass over an empty ledger — this is the reconcile-orphan failure", nq)
		}
	}
}

// TestIntegrationReevaluationRunnableAsAJob wires the pass to the surfaces an
// operator actually reaches: the named worker loop and the SEARCH_RUN_JOB one-off.
func TestIntegrationReevaluationRunnableAsAJob(t *testing.T) {
	env := newTestEnv(t)

	seedSuggestible(t, env, dormantQuery)
	seedSuggestible(t, env, livelyQuery)
	forgetQueryLog(t, env, dormantQuery)

	runWorker(t, env, "suggestible_reeval")

	if _, suggestible, _ := banState(t, env, dormantQuery); suggestible {
		t.Errorf("RunOnce(%q) must apply the pass", "suggestible_reeval")
	}
	if _, suggestible, _ := banState(t, env, livelyQuery); !suggestible {
		t.Errorf("RunOnce(%q) must not touch a still-supported row", "suggestible_reeval")
	}
}
