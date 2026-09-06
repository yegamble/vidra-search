package ranking

import (
	"math"
	"testing"
)

func TestCovisShrunkCosineNoEvidenceIsZero(t *testing.T) {
	if CovisShrunkCosine(0, 10, 10, 10) != 0 {
		t.Errorf("zero co-occurrence must score 0")
	}
	if CovisShrunkCosine(5, 0, 10, 10) != 0 {
		t.Errorf("zero total must score 0")
	}
}

// TestCovisShrunkCosineShrinksLowSupport: with equal cosine geometry, a
// low-support pair scores far below a high-support pair (the shrinkage factor).
func TestCovisShrunkCosineShrinksLowSupport(t *testing.T) {
	// Two pairs each with raw cosine 1 (cooc == sqrt(totI*totJ)), but different
	// support: cooc=1 vs cooc=100.
	low := CovisShrunkCosine(1, 1, 1, 10)        // raw=1, shrink=1/11
	high := CovisShrunkCosine(100, 100, 100, 10) // raw=1, shrink=100/110
	if !approx(low, 1.0/11, 1e-9) {
		t.Errorf("low-support shrunk cosine = %v, want 1/11", low)
	}
	if !approx(high, 100.0/110, 1e-9) {
		t.Errorf("high-support shrunk cosine = %v, want 100/110", high)
	}
	if low >= high {
		t.Errorf("shrinkage must down-weight low support: low=%v high=%v", low, high)
	}
}

// TestCovisBlendMatchesSQLReference reproduces the psql-verified reference case:
// co_watch a-b=20 (totA=21,totB=20), co_search a-b=5 (totA=totB=5) → blend ≈0.5555.
func TestCovisBlendMatchesSQLReference(t *testing.T) {
	cw := CovisShrunkCosine(20, 21, 20, CovisLambdaDefault)
	cs := CovisShrunkCosine(5, 5, 5, CovisLambdaDefault)
	got := CovisBlend(cw, cs)
	if !approx(got, 0.5555, 1e-3) {
		t.Fatalf("blended neighbor score = %v, want ≈0.5555 (SQL reference)", got)
	}
	// The weak a-c pair (co_watch=1, totA=21, totC=1, no co_search) is heavily shrunk.
	weak := CovisBlend(CovisShrunkCosine(1, 21, 1, CovisLambdaDefault), 0)
	if !approx(weak, 0.0139, 1e-3) {
		t.Fatalf("weak pair score = %v, want ≈0.0139 (SQL reference)", weak)
	}
}

func approx(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

// TestCovisBlendFlooredZeroesBelowFloorSources mirrors the k-anonymity floor the
// neighbour rebuild applies: each SOURCE's contribution is zeroed when that
// source's pair support is below the floor, and an edge left at 0 is not
// published at all (the rebuild's `score > 0`).
func TestCovisBlendFlooredZeroesBelowFloorSources(t *testing.T) {
	cw := CovisShrunkCosine(3, 3, 3, CovisLambdaDefault) // 3/13
	cs := CovisShrunkCosine(1, 1, 1, CovisLambdaDefault) // 1/11

	// Both sources clear the floor: the plain blend.
	if got := CovisBlendFloored(cw, cs, 3, 3, 3); !approx(got, CovisBlend(cw, cs), 1e-12) {
		t.Errorf("at the floor the score must be the unchanged blend: got %v want %v", got, CovisBlend(cw, cs))
	}
	// Co-search below the floor: its term contributes nothing, the co-watch term
	// is untouched (0.7 · 3/13 = 0.16154, the A13 fixture's score).
	if got := CovisBlendFloored(cw, cs, 3, 1, 3); !approx(got, 0.7*3.0/13, 1e-12) {
		t.Errorf("a below-floor co-search must contribute nothing: got %v want %v", got, 0.7*3.0/13)
	}
	// Co-watch below the floor: only the co-search term survives.
	if got := CovisBlendFloored(cw, cs, 1, 3, 3); !approx(got, 0.3*cs, 1e-12) {
		t.Errorf("a below-floor co-watch must contribute nothing: got %v want %v", got, 0.3*cs)
	}
	// Neither source clears it: score 0, so the edge is never published.
	if got := CovisBlendFloored(cw, cs, 1, 1, 3); got != 0 {
		t.Errorf("an edge below the floor on both sources must score exactly 0 (unpublishable), got %v", got)
	}
	// A floor of 1 is the pre-ruling behaviour: everything with evidence publishes.
	if got := CovisBlendFloored(cw, cs, 1, 1, 1); !approx(got, CovisBlend(cw, cs), 1e-12) {
		t.Errorf("floor 1 must publish every pair with evidence: got %v want %v", got, CovisBlend(cw, cs))
	}
}
