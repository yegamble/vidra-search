package ranking

import (
	"fmt"
	"math"
	"testing"
)

func TestSmoothedCTRPriorAndMonotonic(t *testing.T) {
	// No evidence → exactly the prior.
	if got := SmoothedCTR(0, 0); math.Abs(got-CTRPrior()) > 1e-12 {
		t.Fatalf("no-evidence CTR = %v, want prior %v", got, CTRPrior())
	}
	// More clicks at fixed impressions → higher CTR; more impressions at fixed
	// clicks → lower CTR (regularization toward prior).
	if SmoothedCTR(5, 10) <= SmoothedCTR(1, 10) {
		t.Errorf("CTR must increase with clicks")
	}
	if SmoothedCTR(1, 100) >= SmoothedCTR(1, 10) {
		t.Errorf("CTR must decrease with impressions at fixed clicks")
	}
	// Converges toward the raw rate with lots of evidence.
	if got := SmoothedCTR(500, 1000); math.Abs(got-0.5) > 0.02 {
		t.Errorf("CTR with strong evidence = %v, want ≈0.5", got)
	}
}

func TestRerankDeterministic(t *testing.T) {
	docs := sampleDocs()
	first := ids(Rerank(docs, DefaultAdvancedWeights))
	for i := 0; i < 25; i++ {
		if got := ids(Rerank(docs, DefaultAdvancedWeights)); !eqIDs(got, first) {
			t.Fatalf("non-deterministic rerank: %v vs %v", got, first)
		}
	}
}

// TestRerankZeroedPersonalizationEqualsSimpleOrdering is the §1.7 invariant: with
// engagement AND personalization zeroed (anonymous / personalized=false), the
// advanced ranker must reproduce the simple ranker's ordering.
func TestRerankZeroedPersonalizationEqualsSimpleOrdering(t *testing.T) {
	// Distinct channels (no creator penalty), varied text/views/age, no
	// engagement, no personalization.
	docs := []Doc{
		{VideoID: "a", ChannelID: "c1", Features: Features{TextRank: 0.2, Views: 10, AgeDays: 100}},
		{VideoID: "b", ChannelID: "c2", Features: Features{TextRank: 0.9, Views: 5, AgeDays: 5}},
		{VideoID: "c", ChannelID: "c3", Features: Features{TextRank: 0.5, Views: 1000, AgeDays: 400}},
		{VideoID: "d", ChannelID: "c4", Features: Features{TextRank: 0.5, TrgmSim: 0.4, Views: 50, AgeDays: 30}},
	}
	advOrder := ids(Rerank(docs, DefaultAdvancedWeights))

	// Expected order via the simple score directly.
	type sc struct {
		id string
		s  float64
	}
	scs := make([]sc, 0, len(docs))
	for _, d := range docs {
		f := d.Features
		scs = append(scs, sc{d.VideoID, SimpleScore(f.TextRank, f.TrgmSim, f.ExactFlags, f.Views, f.AgeDays)})
	}
	// stable sort by score desc, id asc
	for i := 0; i < len(scs); i++ {
		for j := i + 1; j < len(scs); j++ {
			if scs[j].s > scs[i].s || (scs[j].s == scs[i].s && scs[j].id < scs[i].id) {
				scs[i], scs[j] = scs[j], scs[i]
			}
		}
	}
	want := make([]string, len(scs))
	for i, x := range scs {
		want[i] = x.id
	}
	if !eqIDs(advOrder, want) {
		t.Fatalf("advanced (zeroed) order %v != simple order %v", advOrder, want)
	}
}

func TestRerankCreatorPenaltyDemotesThirdSameChannel(t *testing.T) {
	// Four docs from one channel with strong base scores, plus one competitively-
	// scored doc from a different channel. Without the penalty the four hot-channel
	// docs would sweep the top; the creator penalty must demote the 3rd+ hot docs
	// below the other-channel doc while leaving the first two ahead of it.
	docs := []Doc{
		{VideoID: "s1", ChannelID: "hot", Features: Features{TextRank: 0.90}},
		{VideoID: "s2", ChannelID: "hot", Features: Features{TextRank: 0.89}},
		{VideoID: "s3", ChannelID: "hot", Features: Features{TextRank: 0.88}},
		{VideoID: "s4", ChannelID: "hot", Features: Features{TextRank: 0.87}},
		{VideoID: "other", ChannelID: "cool", Features: Features{TextRank: 0.70}},
	}
	order := ids(Rerank(docs, DefaultAdvancedWeights))
	posOther, posS3, posS4 := indexOf(order, "other"), indexOf(order, "s3"), indexOf(order, "s4")
	if posOther > posS3 || posOther > posS4 {
		t.Errorf("3rd/4th same-channel docs must be demoted below the other-channel doc: %v", order)
	}
	// The first two same-channel docs are NOT penalized (stay above other).
	if indexOf(order, "s1") > posOther || indexOf(order, "s2") > posOther {
		t.Errorf("first two same-channel docs must not be penalized: %v", order)
	}
}

func TestModelFeatureVectorOrderAndValues(t *testing.T) {
	names := ModelFeatureNames()
	if len(names) != 8 || names[0] != "text_rank" || names[7] != "language_match" {
		t.Fatalf("unexpected feature names: %v", names)
	}
	f := Features{TextRank: 0.3, TrgmSim: 0.4, ExactFlags: 0.5, Views: math.E - 1, AgeDays: 12,
		Clicks: 2, Impressions: 8, MeaningfulWatches: 1, LanguageMatch: true}
	v := ModelFeatureVector(f)
	if len(v) != len(names) {
		t.Fatalf("vector len %d != names len %d", len(v), len(names))
	}
	if math.Abs(v[3]-1.0) > 1e-9 { // log_views = ln(1+(e-1)) = 1
		t.Errorf("log_views = %v, want 1.0", v[3])
	}
	if math.Abs(v[5]-SmoothedCTR(2, 8)) > 1e-12 {
		t.Errorf("smoothed_ctr = %v, want %v", v[5], SmoothedCTR(2, 8))
	}
	if v[7] != 1.0 {
		t.Errorf("language_match = %v, want 1.0", v[7])
	}
}

// --- helpers + benchmark ---

func sampleDocs() []Doc {
	docs := make([]Doc, 0, 20)
	for i := 0; i < 20; i++ {
		docs = append(docs, Doc{
			VideoID:   fmt.Sprintf("v%02d", i),
			ChannelID: fmt.Sprintf("ch%d", i%5),
			Features: Features{
				TextRank: float64(i%7) / 7, TrgmSim: float64(i%3) / 3,
				Views: float64(i * 13), AgeDays: float64(i * 3),
				Clicks: float64(i % 4), Impressions: float64(i % 9),
				PersonalAffinity: float64(i % 6), SessionIntent: float64(i % 2),
			},
		})
	}
	return docs
}

func ids(r []Ranked) []string {
	out := make([]string, len(r))
	for i, x := range r {
		out[i] = x.VideoID
	}
	return out
}

func eqIDs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func indexOf(s []string, want string) int {
	for i, v := range s {
		if v == want {
			return i
		}
	}
	return -1
}

// BenchmarkRerankHeuristic500 proves the heuristic rerank of 500 candidates stays
// well under the 10ms/rerank serving budget (§ serving perf).
func BenchmarkRerankHeuristic500(b *testing.B) {
	docs := make([]Doc, 500)
	for i := range docs {
		docs[i] = Doc{
			VideoID:   fmt.Sprintf("v%03d", i),
			ChannelID: fmt.Sprintf("ch%d", i%50),
			Features: Features{
				TextRank: float64(i%97) / 97, TrgmSim: float64(i%31) / 31,
				ExactFlags: float64(i%3) * 0.5, Views: float64(i * 7), AgeDays: float64(i % 365),
				Clicks: float64(i % 11), Impressions: float64(i % 53),
				MeaningfulWatches: float64(i % 5),
				PersonalAffinity:  float64(i % 17), ChannelAffinity: float64(i % 7), SessionIntent: float64(i % 3),
			},
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = Rerank(docs, DefaultAdvancedWeights)
	}
}
