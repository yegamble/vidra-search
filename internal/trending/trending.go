// Package trending holds the pure math behind decayed-counter trending and its
// manipulation-resistance gates (§1.3, §1.9 trending_sweeper; algorithms report
// "Trending"). It has no I/O so the decay curve and the Wilson/distinct-user
// gates are exhaustively unit-testable. The Redis mechanics (ZSETs, HLLs,
// per-user caps) live in internal/cache; this package decides what those numbers
// mean.
package trending

import "math"

// DefaultZ is the Wilson score z for a ~95% one-sided lower bound.
const DefaultZ = 1.96

// Scored is one trending item with its (decayed) score.
type Scored struct {
	Item  string  `json:"item"`
	Score float64 `json:"score"`
}

// DecayFactor returns 2^(-elapsed/halfLife): the multiplier that decays a counter
// observed elapsedSeconds ago under an exponential half-life. It is 1 at
// elapsed==0, 0.5 at one half-life, and clamps a negative elapsed (clock skew) to
// no decay. A non-positive half-life disables decay (factor 1).
func DecayFactor(elapsedSeconds, halfLifeSeconds float64) float64 {
	if halfLifeSeconds <= 0 {
		return 1
	}
	if elapsedSeconds <= 0 {
		return 1
	}
	return math.Exp2(-elapsedSeconds / halfLifeSeconds)
}

// Decay applies DecayFactor to value.
func Decay(value, elapsedSeconds, halfLifeSeconds float64) float64 {
	return value * DecayFactor(elapsedSeconds, halfLifeSeconds)
}

// WilsonLowerBound is the lower bound of the Wilson score interval for a Bernoulli
// proportion pos/n at confidence parameter z. It is the standard confidence-aware
// rate estimate (Wilson 1927; popularised by Miller's "how not to sort by average
// rating"): with little evidence the bound is pulled far below the naive rate, and
// it converges to pos/n as n grows. Returns 0 for n<=0.
func WilsonLowerBound(pos, n, z float64) float64 {
	if n <= 0 || pos <= 0 {
		return 0
	}
	phat := pos / n
	z2 := z * z
	denom := 1 + z2/n
	centre := phat + z2/(2*n)
	margin := z * math.Sqrt((phat*(1-phat)+z2/(4*n))/n)
	lb := (centre - margin) / denom
	if lb < 0 {
		return 0
	}
	return lb
}

// Gate rejection reasons (also the bounded metric label values).
const (
	ReasonOK            = ""
	ReasonDistinctUsers = "distinct_users"
	ReasonWilsonMinVol  = "wilson_min_volume"
)

// EvaluateGate decides whether an item may be exposed as trending. Two guards run
// before exposure (algorithms report):
//
//  1. Distinct-user floor: at least minUsers genuinely distinct users (HLL) must
//     have contributed. This alone defeats a single user spamming one item — their
//     distinct count stays 1.
//  2. Wilson min-volume: treat distinct users as "successes" over total
//     contributions n; the Wilson lower bound of that fraction must clear
//     wilsonFloor. A small number of users generating a large raw volume (bots)
//     has a low distinct/total fraction and is rejected with confidence.
//
// It returns (true, ReasonOK) when both pass, else (false, reason) naming the
// first failing guard.
func EvaluateGate(distinctUsers int, total float64, minUsers int, wilsonFloor, z float64) (bool, string) {
	if distinctUsers < minUsers {
		return false, ReasonDistinctUsers
	}
	if WilsonLowerBound(float64(distinctUsers), total, z) < wilsonFloor {
		return false, ReasonWilsonMinVol
	}
	return true, ReasonOK
}
