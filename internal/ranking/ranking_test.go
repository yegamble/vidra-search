package ranking

import (
	"math"
	"testing"
)

func texts(s []Suggestion) []string {
	out := make([]string, len(s))
	for i, x := range s {
		out[i] = x.Text
	}
	return out
}

func TestBlendExactBeatsFuzzyAndOrdersByPopularity(t *testing.T) {
	cands := []Candidate{
		{Text: "golang tutorial", Kind: KindVideo, Source: SourceDoc, ExactPrefix: true, Popularity: 100},
		{Text: "golang basics", Kind: KindVideo, Source: SourceDoc, ExactPrefix: true, Popularity: 500},
		{Text: "golfing", Kind: KindVideo, Source: SourceDoc, ExactPrefix: false, Popularity: 9000}, // fuzzy
	}
	got := Blend(cands, 10, DefaultWeights)
	// Both exact-prefix items must precede the fuzzy one regardless of the
	// fuzzy item's higher raw popularity (prefix quality dominates the blend).
	if got[len(got)-1].Text != "golfing" {
		t.Fatalf("fuzzy match should rank last, got order %v", texts(got))
	}
	// Between the two exact matches, higher doc popularity wins.
	if got[0].Text != "golang basics" {
		t.Fatalf("higher-popularity exact match should rank first, got %v", texts(got))
	}
}

func TestBlendDedupesCaseInsensitively(t *testing.T) {
	cands := []Candidate{
		{Text: "Golang", Kind: KindVideo, Source: SourceDoc, ExactPrefix: true, Popularity: 10},
		{Text: "golang", Kind: KindQuery, Source: SourceQuery, ExactPrefix: true, Popularity: 10},
		{Text: "GOLANG", Kind: KindTag, Source: SourceDoc, ExactPrefix: true, Popularity: 5},
	}
	got := Blend(cands, 10, DefaultWeights)
	if len(got) != 1 {
		t.Fatalf("expected 1 deduped suggestion, got %d: %v", len(got), texts(got))
	}
}

func TestBlendPersonalFlagSurvivesDedupe(t *testing.T) {
	cands := []Candidate{
		{Text: "my query", Kind: KindVideo, Source: SourceDoc, ExactPrefix: true, Popularity: 100},
		{Text: "my query", Kind: KindHistory, Source: SourceHistory, ExactPrefix: true, IsPersonal: true, Popularity: 1},
	}
	got := Blend(cands, 10, DefaultWeights)
	if len(got) != 1 || !got[0].IsPersonal {
		t.Fatalf("personal flag must survive dedupe, got %+v", got)
	}
}

func TestBlendReservesDocSlot(t *testing.T) {
	// Fill the window with high-popularity query candidates, plus one weak
	// doc-derived candidate that would otherwise be evicted.
	var cands []Candidate
	for i := 0; i < 5; i++ {
		cands = append(cands, Candidate{
			Text: string(rune('a'+i)) + "-query", Kind: KindQuery, Source: SourceQuery,
			ExactPrefix: true, Popularity: 1000,
		})
	}
	cands = append(cands, Candidate{Text: "zzz-doc", Kind: KindVideo, Source: SourceDoc, ExactPrefix: true, Popularity: 1})
	got := Blend(cands, 3, DefaultWeights)
	found := false
	for _, s := range got {
		if s.Text == "zzz-doc" {
			found = true
		}
	}
	if !found {
		t.Fatalf("doc-derived slot should be reserved, got %v", texts(got))
	}
}

func TestBlendTrendingBoost(t *testing.T) {
	// Two otherwise-identical doc candidates; the trending one ranks first.
	cands := []Candidate{
		{Text: "quiet topic", Kind: KindQuery, Source: SourceDoc, ExactPrefix: true, Popularity: 100},
		{Text: "hot topic", Kind: KindQuery, Source: SourceDoc, ExactPrefix: true, Popularity: 100, Trending: true},
	}
	got := Blend(cands, 10, DefaultWeights)
	if got[0].Text != "hot topic" {
		t.Fatalf("trending candidate should rank first, got %v", texts(got))
	}
}

func TestBlendAggregateAndPersonalStreams(t *testing.T) {
	// An aggregate (global) query, a doc-derived title, and a personal-history
	// entry all blend; the personal entry keeps is_personal + type=history.
	cands := []Candidate{
		{Text: "golang jobs", Kind: KindQuery, Source: SourceQuery, ExactPrefix: true, Popularity: 900},
		{Text: "golang basics", Kind: KindQuery, Source: SourceDoc, ExactPrefix: true, Popularity: 10},
		{Text: "golang generics", Kind: KindHistory, Source: SourceHistory, ExactPrefix: true, IsPersonal: true, Popularity: 3},
	}
	got := Blend(cands, 10, DefaultWeights)
	if len(got) != 3 {
		t.Fatalf("expected all three streams represented, got %v", texts(got))
	}
	var personal *Suggestion
	for i := range got {
		if got[i].Text == "golang generics" {
			personal = &got[i]
		}
	}
	if personal == nil || !personal.IsPersonal || personal.Type != KindHistory {
		t.Fatalf("personal entry must be is_personal + type=history, got %+v", personal)
	}
}

func TestBlendReservesPersonalSlot(t *testing.T) {
	// Fill the window with strong aggregate candidates plus one weak personal-
	// history entry that must be reserved into the top slots.
	var cands []Candidate
	for i := 0; i < 5; i++ {
		cands = append(cands, Candidate{
			Text: string(rune('a'+i)) + "-agg", Kind: KindQuery, Source: SourceQuery,
			ExactPrefix: true, Popularity: 1000,
		})
	}
	cands = append(cands, Candidate{
		Text: "zzz-personal", Kind: KindHistory, Source: SourceHistory,
		ExactPrefix: true, IsPersonal: true, Popularity: 1,
	})
	got := Blend(cands, 3, DefaultWeights)
	found := false
	for _, s := range got {
		if s.Text == "zzz-personal" {
			found = true
		}
	}
	if !found {
		t.Fatalf("a personal (history) slot must be reserved, got %v", texts(got))
	}
}

func TestBlendRespectsLimit(t *testing.T) {
	var cands []Candidate
	for i := 0; i < 20; i++ {
		cands = append(cands, Candidate{Text: string(rune('a' + i)), Kind: KindVideo, Source: SourceDoc, ExactPrefix: true, Popularity: float64(i)})
	}
	if got := Blend(cands, 5, DefaultWeights); len(got) != 5 {
		t.Fatalf("expected 5, got %d", len(got))
	}
}

func TestBlendDeterministic(t *testing.T) {
	cands := []Candidate{
		{Text: "b", Source: SourceDoc, ExactPrefix: true, Popularity: 10},
		{Text: "a", Source: SourceDoc, ExactPrefix: true, Popularity: 10},
		{Text: "c", Source: SourceDoc, ExactPrefix: true, Popularity: 10},
	}
	first := texts(Blend(cands, 10, DefaultWeights))
	for i := 0; i < 20; i++ {
		if got := texts(Blend(cands, 10, DefaultWeights)); !equal(got, first) {
			t.Fatalf("non-deterministic order: %v vs %v", got, first)
		}
	}
	// Equal-score, equal-prefix ties break lexically.
	if first[0] != "a" || first[1] != "b" || first[2] != "c" {
		t.Fatalf("expected lexical tie-break a,b,c, got %v", first)
	}
}

func TestSimpleWeightsSumToOne(t *testing.T) {
	sum := SimpleWTextRank + SimpleWTrgm + SimpleWExact + SimpleWViews + SimpleWFresh
	if math.Abs(sum-1.0) > 1e-9 {
		t.Fatalf("simple score weights must sum to 1.0, got %v", sum)
	}
}

func TestSimpleScoreFreshnessHalfLife(t *testing.T) {
	// At one half-life the freshness term must be half its fresh value.
	fresh := SimpleScore(0, 0, 0, 0, 0)
	half := SimpleScore(0, 0, 0, 0, SimpleFreshnessHalfLifeDays)
	if math.Abs((fresh-half)-(SimpleWFresh*0.5)) > 1e-9 {
		t.Fatalf("freshness half-life decay incorrect: fresh=%v half=%v", fresh, half)
	}
}

func equal(a, b []string) bool {
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
