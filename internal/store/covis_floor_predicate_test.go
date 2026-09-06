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
// co-visitation window. It must appear identically in all FOUR places
// covisitation.sql pairs events: the two accumulators that count the pair, and
// the two floor-support CTEs that decide whether that pair may be published.
//
// If the accumulator and its floor disagree about what a co-visit IS, the floor
// gates a different set of pairs than the counters counted — which shows up as
// edges that are published with too little support (the floor pairs more loosely
// than the counter) or real edges silently missing (it pairs more tightly), and
// in neither case does anything fail.
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

// TestCovisFloorPairsExactlyLikeTheAccumulators pins the other half: the floor's
// notion of a co-visit is the accumulators' notion of a co-visit.
func TestCovisFloorPairsExactlyLikeTheAccumulators(t *testing.T) {
	if got := strings.Count(covisSQL(t), covisPairPredicate); got != 4 {
		t.Errorf("covisitation.sql contains the shared pairing predicate %d time(s), want 4 "+
			"(AccumulateCoWatch, AccumulateCoSearch, and the co-watch + co-search floor support CTEs).\n"+
			"Expected exactly:\n%s", got, covisPairPredicate)
	}
}
