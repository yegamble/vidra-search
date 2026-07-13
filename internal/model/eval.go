package model

import (
	"math"
	"sort"
)

// Ranking-quality metrics used by shadow evaluation (§1.9). Both take the graded
// relevance labels IN THE ORDER a ranker placed them, so the same helpers score
// the production ordering, the shadow model, and the heuristic re-rank.

// dcgAt is the Discounted Cumulative Gain over the first k positions using the
// standard 2^rel − 1 gain and log2(i+2) discount.
func dcgAt(relsInOrder []float64, k int) float64 {
	sum := 0.0
	for i := 0; i < len(relsInOrder) && i < k; i++ {
		sum += (math.Pow(2, relsInOrder[i]) - 1) / math.Log2(float64(i+2))
	}
	return sum
}

// NDCGAt is the Normalized DCG@k: dcg over the given order divided by the ideal
// dcg (best possible order of the same labels). Returns 0 when there is no
// positive signal (ideal dcg is 0).
func NDCGAt(relsInOrder []float64, k int) float64 {
	dcg := dcgAt(relsInOrder, k)
	ideal := append([]float64(nil), relsInOrder...)
	sort.Sort(sort.Reverse(sort.Float64Slice(ideal)))
	idcg := dcgAt(ideal, k)
	if idcg == 0 {
		return 0
	}
	return dcg / idcg
}

// MRRAt is the Mean Reciprocal Rank contribution of a single ranked list: the
// reciprocal of the 1-based position of the first relevant (rel ≥ 1) item within
// the first k, or 0 if none.
func MRRAt(relsInOrder []float64, k int) float64 {
	for i := 0; i < len(relsInOrder) && i < k; i++ {
		if relsInOrder[i] >= 1 {
			return 1.0 / float64(i+1)
		}
	}
	return 0
}
