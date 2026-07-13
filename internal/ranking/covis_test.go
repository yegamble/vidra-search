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
