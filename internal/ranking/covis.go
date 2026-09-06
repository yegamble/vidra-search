package ranking

import "math"

// Co-visitation neighbor scoring math (§1.9 covis_rollup). These pure functions
// are the authoritative mirror of the RebuildCovisNeighbors SQL: the live rebuild
// runs the SQL, and these document + unit-test the exact formula (the same
// pattern as SimpleScore mirroring the SearchSimple SQL).

// CovisLambdaDefault is the shrinkage λ (algorithms report ≈10): it damps
// low-support pairs so a single co-occurrence does not read as strong similarity.
const CovisLambdaDefault = 10.0

// CovisBlendCoWatch / CovisBlendCoSearch are the blend weights combining the two
// co-visitation matrices into one neighbor score.
const (
	CovisBlendCoWatch  = 0.7
	CovisBlendCoSearch = 0.3
)

// CovisShrunkCosine is the shrunk cosine similarity of one co-visitation matrix:
//
//	raw    = cooc / sqrt(totI * totJ)
//	shrunk = raw * cooc / (cooc + lambda)
//
// where cooc is the (i,j) co-occurrence count and totI/totJ are the summed
// co-occurrence mass of items i and j in that matrix. Returns 0 for non-positive
// inputs (no evidence).
func CovisShrunkCosine(cooc, totI, totJ, lambda float64) float64 {
	if cooc <= 0 || totI <= 0 || totJ <= 0 {
		return 0
	}
	raw := cooc / math.Sqrt(totI*totJ)
	return raw * (cooc / (cooc + lambda))
}

// CovisBlend blends the co_watch and co_search shrunk cosines into the final
// item_neighbors score (source='blend').
func CovisBlend(coWatchCosine, coSearchCosine float64) float64 {
	return CovisBlendCoWatch*coWatchCosine + CovisBlendCoSearch*coSearchCosine
}

// CovisBlendFloored is the PUBLISHED neighbor score: the blend with each source's
// contribution zeroed when fewer than minSubjects distinct subjects co-visited
// that pair through that source. minSubjects is autosuggest's k-anonymity floor
// (`minimum_query_user_count`), reused rather than duplicated — item_neighbors is
// a globally-served index, so publishing an edge one person's session produced
// tells every viewer something about that person.
//
// Shrinkage is NOT this gate: cooc/(cooc+lambda) ranks a one-person pair low, it
// still publishes it, and on a quiet instance low is first.
//
// The two sources are floored independently so a below-floor co-search can
// neither publish an edge alone nor inflate one the co-watch half earned. A pair
// below the floor on both scores exactly 0, and the rebuild publishes only
// score > 0 — so 0 here means "not in the index at all".
func CovisBlendFloored(coWatchCosine, coSearchCosine float64, coWatchSubjects, coSearchSubjects, minSubjects int) float64 {
	if coWatchSubjects < minSubjects {
		coWatchCosine = 0
	}
	if coSearchSubjects < minSubjects {
		coSearchCosine = 0
	}
	return CovisBlend(coWatchCosine, coSearchCosine)
}
