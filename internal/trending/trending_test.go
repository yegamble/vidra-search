package trending

import (
	"math"
	"testing"
)

func approx(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

func TestDecayFactorHalfLifeBoundaries(t *testing.T) {
	const hl = 3600.0
	cases := []struct {
		elapsed, want float64
	}{
		{0, 1.0},        // no time passed
		{-100, 1.0},     // clock skew clamps to no decay
		{hl, 0.5},       // one half-life halves the value
		{2 * hl, 0.25},  // two half-lives quarter it
		{3 * hl, 0.125}, // three
	}
	for _, c := range cases {
		if got := DecayFactor(c.elapsed, hl); !approx(got, c.want, 1e-9) {
			t.Errorf("DecayFactor(%v, %v) = %v, want %v", c.elapsed, hl, got, c.want)
		}
	}
	if got := DecayFactor(1000, 0); got != 1 {
		t.Errorf("non-positive half-life must disable decay, got %v", got)
	}
}

func TestDecayMonotonicNonIncreasing(t *testing.T) {
	const hl = 100.0
	prev := math.Inf(1)
	for e := 0.0; e <= 1000; e += 25 {
		got := Decay(10, e, hl)
		if got > prev+1e-12 {
			t.Fatalf("decay must be non-increasing in elapsed: at %v got %v > prev %v", e, got, prev)
		}
		prev = got
	}
}

func TestWilsonLowerBound(t *testing.T) {
	if got := WilsonLowerBound(0, 100, DefaultZ); got != 0 {
		t.Errorf("zero successes must give 0, got %v", got)
	}
	if got := WilsonLowerBound(5, 0, DefaultZ); got != 0 {
		t.Errorf("zero trials must give 0, got %v", got)
	}
	// The lower bound never exceeds the naive proportion and tightens toward it
	// as evidence grows.
	small := WilsonLowerBound(1, 1, DefaultZ)
	large := WilsonLowerBound(1000, 1000, DefaultZ)
	if !(small < large) {
		t.Errorf("more evidence at the same rate must raise the lower bound: small=%v large=%v", small, large)
	}
	if small > 1 || large > 1 {
		t.Errorf("lower bound must be <= 1: small=%v large=%v", small, large)
	}
	// Spam shape (1 distinct user out of 1000 events) is pinned near zero.
	if got := WilsonLowerBound(1, 1000, DefaultZ); got > 0.02 {
		t.Errorf("a 1/1000 distinct-user fraction must be near zero, got %v", got)
	}
}

func TestEvaluateGate(t *testing.T) {
	const minUsers, floor = 3, 0.10

	// Manipulation: one user, huge volume → rejected on the distinct-user gate.
	if ok, reason := EvaluateGate(1, 1000, minUsers, floor, DefaultZ); ok || reason != ReasonDistinctUsers {
		t.Errorf("single-user spam must fail on distinct users, got ok=%v reason=%q", ok, reason)
	}
	// Enough distinct users but a thin, bot-like volume fraction → Wilson gate.
	if ok, reason := EvaluateGate(3, 5000, minUsers, floor, DefaultZ); ok || reason != ReasonWilsonMinVol {
		t.Errorf("thin distinct fraction must fail on Wilson, got ok=%v reason=%q", ok, reason)
	}
	// Healthy: many distinct users, most contributions distinct → passes.
	if ok, reason := EvaluateGate(50, 80, minUsers, floor, DefaultZ); !ok || reason != ReasonOK {
		t.Errorf("healthy trend must pass, got ok=%v reason=%q", ok, reason)
	}
}
