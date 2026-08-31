// Source-level guard on the ONE invariant that ties rollups.sql to
// reevaluation.sql. It needs no database, so it runs in the fast `make ci` lane
// where the integration suite does not.
package store_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// floorCount is the anonymous half of the k-anonymity floor, byte-for-byte as it
// must appear in BOTH query files. Update it here only together with both.
const floorCount = "count(DISTINCT ql.user_id)\n" +
	"              + count(DISTINCT CASE WHEN ql.user_id IS NULL THEN COALESCE(ql.subject_id, ql.session_id) END)"

// TestFloorPredicateIsSharedByRollupAndReevaluation pins the construction PR #31
// chose deliberately: the daily re-evaluation pass is the rollup's recount with
// the batch INNER JOIN removed, so the two can never disagree about who counts.
// If they diverge, every row one pass demotes the other promotes back, and
// `suggestible` flaps on every cycle — which looks to an operator like autosuggest
// randomly gaining and losing entries, with nothing in the logs to explain it.
//
// An integration test can only catch a divergence the fixture happens to
// straddle. This catches every divergence, including one nobody thought to seed.
func TestFloorPredicateIsSharedByRollupAndReevaluation(t *testing.T) {
	// reevaluation.sql carries the count twice (the dry run and the apply), and
	// they must match each other as well as the rollup.
	for _, tc := range []struct {
		path string
		want int
	}{
		{filepath.Join("queries", "rollups.sql"), 1},
		{filepath.Join("queries", "reevaluation.sql"), 2},
	} {
		src, err := os.ReadFile(tc.path)
		if err != nil {
			t.Fatalf("read %s: %v", tc.path, err)
		}
		if got := strings.Count(string(src), floorCount); got != tc.want {
			t.Errorf("%s contains the shared floor count %d time(s), want %d.\n"+
				"The rollup and the re-evaluation pass MUST count the distinct-user floor identically; "+
				"change one and you must change the other in the same commit.\nExpected exactly:\n%s",
				tc.path, got, tc.want, floorCount)
		}
	}
}
