// Source-level guards on the co-visitation k-anonymity floor. They need no
// database, so they run in the fast `make ci` lane where the integration suite
// does not — the same construction as floor_predicate_test.go, for the same
// reason: an integration test can only catch a divergence its fixture happens to
// straddle, while a source guard catches every divergence including one nobody
// thought to seed.
package store_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// covisPairPredicate is the sessionized pairing predicate: a NEW event is paired
// only with an EARLIER event of the same session on a DIFFERENT video inside the
// co-visitation window. It must appear identically in BOTH places covisitation.sql
// pairs events — the co-watch CTE and the co-search CTE.
//
// It used to have to appear four times, because the two cumulative accumulators
// paired the ledger to COUNT a pair and the two support CTEs paired it again to
// decide whether that pair could be published; a disagreement between them
// published edges with too little support, or silently dropped real ones, and
// nothing failed. The counts now come out of the same CTE as the support, so that
// divergence is impossible by construction and this guard is left with the one
// that is not: the co-watch and co-search halves must pair the ledger the same
// way, apart from co-search's extra same-query clause.
const covisPairPredicate = "     AND p.id < n.id\n" +
	"     AND p.video_id <> n.video_id\n" +
	"     AND abs(EXTRACT(EPOCH FROM (n.occurred_at - p.occurred_at))) <= @window_seconds::double precision"

func covisSQL(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("queries", "covisitation.sql"))
	if err != nil {
		t.Fatalf("read covisitation.sql: %v", err)
	}
	return string(src)
}

// TestCovisFloorCountsIdenticallyToAutosuggest pins the ruling: co-visitation
// neighbours carry the SAME k-anonymity floor as autosuggest and trending, and
// "the same floor" means the same identity is counted — the account id when
// signed in, else core's server-derived subject_id, else the client's session id.
// The expression is autosuggest's, byte-for-byte, with only the table alias
// changed, so the two can never drift into counting different people.
func TestCovisFloorCountsIdenticallyToAutosuggest(t *testing.T) {
	// floorCount is defined in floor_predicate_test.go and is the expression
	// rollups.sql and reevaluation.sql share; `ql` is the query_log alias there,
	// `n` is the anchor behavior_events alias here.
	want := strings.ReplaceAll(floorCount, "ql.", "n.")
	if got := strings.Count(covisSQL(t), want); got != 2 {
		t.Errorf("covisitation.sql contains autosuggest's floor count %d time(s), want 2 "+
			"(one for co-watch support, one for co-search support).\n"+
			"The co-visitation floor MUST count the same identity autosuggest counts; "+
			"change one and you must change the other in the same commit.\nExpected exactly:\n%s",
			got, want)
	}
}

// TestCovisPairsBothSourcesIdentically pins the other half: the two blended
// sources agree on what a co-visit IS.
func TestCovisPairsBothSourcesIdentically(t *testing.T) {
	if got := strings.Count(covisSQL(t), covisPairPredicate); got != 2 {
		t.Errorf("covisitation.sql contains the shared pairing predicate %d time(s), want 2 "+
			"(the co-watch and co-search pair CTEs, each producing that source's counts AND "+
			"its floor support).\nExpected exactly:\n%s", got, covisPairPredicate)
	}
}

// TestCovisRebuildReadsOnlyTheRetainedLedger pins the retention ruling at the
// source level: the served neighbour index is a function of `behavior_events` and
// of nothing else. `co_watch` / `co_search` were cumulative and unpruned, so any
// reference to them from this file is a co-occurrence number that outlives the
// events it was derived from — which is the whole defect. An integration test can
// only catch that on a fixture whose retention boundary it happens to straddle;
// this catches the reference itself, including one added for the normalization
// mass, where the effect on any single score is small enough to look like drift.
func TestCovisRebuildReadsOnlyTheRetainedLedger(t *testing.T) {
	// Comments are stripped first: the file explains at length what the counters
	// were and why they are gone, and a guard that read its own prose as a
	// reference would be unfixable without deleting the explanation.
	lines := strings.Split(covisSQL(t), "\n")
	for i, line := range lines {
		if c := strings.Index(line, "--"); c >= 0 {
			lines[i] = line[:c]
		}
	}
	code := strings.Join(lines, "\n")
	for _, table := range []string{"search.co_watch", "search.co_search"} {
		if strings.Contains(code, table) {
			t.Errorf("covisitation.sql references %s. The co-visitation counters are RETIRED: "+
				"they are cumulative and nothing prunes them, so a score read from them keeps "+
				"counting co-visits retention (or a user's purge) has already deleted. Pair "+
				"counts, normalization mass and floor support all come from the retained "+
				"behavior_events ledger.", table)
		}
	}
}
