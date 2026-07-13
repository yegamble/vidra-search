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
