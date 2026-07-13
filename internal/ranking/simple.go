package ranking

import "math"

// Simple-mode search score weights (§1.7). The score itself is computed in SQL
// (SearchSimple) for a single-round-trip ranked scan; these constants are the
// authoritative documentation of that formula and are guarded by a unit test so
// a change to the SQL weights without updating them (or vice versa) is caught.
//
//	score = W_TextRank * ts_rank_cd
//	      + W_Trgm     * trigram_similarity(title, q)
//	      + W_Exact    * exact_match_flags   (title 1.0, channel 0.5, tag 0.5)
//	      + W_Views    * ln(1+views)/20
//	      + W_Fresh    * exp(-ln2 * age_days / 30)
const (
	SimpleWTextRank = 0.5
	SimpleWTrgm     = 0.2
	SimpleWExact    = 0.1
	SimpleWViews    = 0.1
	SimpleWFresh    = 0.1

	// SimpleFreshnessHalfLifeDays is the freshness decay half-life.
	SimpleFreshnessHalfLifeDays = 30.0
	// SimpleViewsLogDivisor normalizes ln(1+views) into roughly [0,1].
	SimpleViewsLogDivisor = 20.0
	// SimpleTrgmCandidateThreshold is the minimum trigram similarity for a title
	// to enter the candidate set (recall), independent of the score.
	SimpleTrgmCandidateThreshold = 0.3
)

// SimpleScore recomputes the SQL score in Go for a single document. It is a
// documentation/verification mirror of the SearchSimple SQL, exercised by the
// unit tests; the live ranking path runs the SQL, not this function.
func SimpleScore(tsRank, trgmSim, exactFlags, views, ageDays float64) float64 {
	return SimpleWTextRank*tsRank +
		SimpleWTrgm*trgmSim +
		SimpleWExact*exactFlags +
		SimpleWViews*(math.Log(1+views)/SimpleViewsLogDivisor) +
		SimpleWFresh*math.Exp(-math.Ln2*ageDays/SimpleFreshnessHalfLifeDays)
}
