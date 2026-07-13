package ranking

import (
	"math"
	"testing"
)

func tokens(ts ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(ts))
	for _, t := range ts {
		m[t] = struct{}{}
	}
	return m
}

// TestMMRDiversifiesTopics: two near-duplicate high-relevance docs plus a slightly
// lower-relevance doc on a different topic. With λ<1 the diverse doc must be
// promoted ahead of the second near-duplicate.
func TestMMRDiversifiesTopics(t *testing.T) {
	docs := []MMRDoc{
		{VideoID: "a", Relevance: 1.00, Tokens: tokens("cat:music", "tag:live")},
		{VideoID: "b", Relevance: 0.98, Tokens: tokens("cat:music", "tag:live")}, // near-dup of a
		{VideoID: "c", Relevance: 0.90, Tokens: tokens("cat:cooking", "tag:recipe")},
	}
	order := MMR(docs, 0.7, 3)
	if order[0] != "a" {
		t.Fatalf("most relevant should lead: %v", order)
	}
	if order[1] != "c" {
		t.Fatalf("diverse doc c should be promoted over near-duplicate b: %v", order)
	}
}

// TestMMRLambdaOneIsPureRelevance: λ=1 ignores similarity → strict relevance order.
func TestMMRLambdaOneIsPureRelevance(t *testing.T) {
	docs := []MMRDoc{
		{VideoID: "a", Relevance: 1.00, Tokens: tokens("cat:music")},
		{VideoID: "b", Relevance: 0.98, Tokens: tokens("cat:music")},
		{VideoID: "c", Relevance: 0.90, Tokens: tokens("cat:cooking")},
	}
	order := MMR(docs, 1.0, 3)
	want := []string{"a", "b", "c"}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("λ=1 must be pure relevance order %v, got %v", want, order)
		}
	}
}

func TestMMRDeterministicAndTieBreak(t *testing.T) {
	docs := []MMRDoc{
		{VideoID: "z", Relevance: 0.5, Tokens: tokens("x")},
		{VideoID: "a", Relevance: 0.5, Tokens: tokens("y")},
		{VideoID: "m", Relevance: 0.5, Tokens: tokens("z")},
	}
	first := MMR(docs, 0.7, 3)
	if first[0] != "a" {
		t.Fatalf("equal-relevance tie must break by id asc, got %v", first)
	}
	for i := 0; i < 20; i++ {
		got := MMR(docs, 0.7, 3)
		for j := range got {
			if got[j] != first[j] {
				t.Fatalf("MMR not deterministic: %v vs %v", got, first)
			}
		}
	}
}

func TestMMRRespectsK(t *testing.T) {
	docs := []MMRDoc{
		{VideoID: "a", Relevance: 1}, {VideoID: "b", Relevance: 0.9}, {VideoID: "c", Relevance: 0.8},
	}
	if got := MMR(docs, 0.7, 2); len(got) != 2 {
		t.Fatalf("expected 2 results, got %d", len(got))
	}
	if got := MMR(docs, 0.7, 10); len(got) != 3 {
		t.Fatalf("k>n must clamp to n, got %d", len(got))
	}
}

// TestExplorationSlotDeterministic: the same seed always yields the same decision
// and index, and different seeds vary.
func TestExplorationSlotDeterministic(t *testing.T) {
	for _, seed := range []string{"user-1", "sess-abc", "user-2"} {
		fire1, idx1 := ExplorationSlot(seed, 0.1, 10)
		for i := 0; i < 10; i++ {
			fire2, idx2 := ExplorationSlot(seed, 0.1, 10)
			if fire1 != fire2 || idx1 != idx2 {
				t.Fatalf("ExplorationSlot not deterministic for %q: (%v,%d) vs (%v,%d)", seed, fire1, idx1, fire2, idx2)
			}
		}
	}
}

func TestExplorationSlotGuards(t *testing.T) {
	if fire, _ := ExplorationSlot("s", 0.1, 0); fire {
		t.Errorf("empty pool must never fire")
	}
	if fire, _ := ExplorationSlot("s", 0, 10); fire {
		t.Errorf("epsilon 0 must never fire")
	}
	// ε=1 always fires and returns an in-range index.
	fire, idx := ExplorationSlot("s", 1.0, 5)
	if !fire || idx < 0 || idx >= 5 {
		t.Errorf("epsilon 1 must fire with in-range index, got fire=%v idx=%d", fire, idx)
	}
}

// TestExplorationSlotDistribution: over many distinct seeds, the fire rate is
// approximately ε (deterministic per seed, but uniform across the population).
func TestExplorationSlotDistribution(t *testing.T) {
	const n = 20000
	const eps = 0.1
	fires := 0
	for i := 0; i < n; i++ {
		if fire, _ := ExplorationSlot("subject-"+itoa(i), eps, 8); fire {
			fires++
		}
	}
	rate := float64(fires) / n
	if math.Abs(rate-eps) > 0.02 {
		t.Errorf("fire rate %.4f not ≈ ε=%.2f", rate, eps)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}
