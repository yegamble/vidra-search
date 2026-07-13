package ranking

import (
	"math"
	"sort"
)

// Advanced-mode reranker (§1.7 stage-2, §1.8). A hand-tuned LINEAR model over
// interpretable features, kept pure and deterministic so it is exhaustively
// unit-testable and cheap enough to rerank 500 candidates well under budget. The
// learned LightGBM ranker (internal/model) is an optional drop-in that consumes
// the SAME Features; this heuristic is always available and is the fallback.
//
// Design invariant (tested): with engagement AND personalization zeroed and no
// language match, the per-doc score reduces to exactly SimpleScore, so an
// anonymous / personalized=false advanced request produces the same ordering as
// the simple ranker. Every advanced-only term is 0 in that baseline:
//   - the smoothed-CTR term subtracts its own prior, so no engagement ⇒ 0;
//   - the meaningful-watch-rate term is 0 with no clicks;
//   - personal/channel/session affinity are 0 for anonymous requests;
//   - the language term is 0 with no query language.

// AdvancedWeights are the linear blend coefficients for the advanced ranker. The
// base (text/views/freshness) sub-score reuses the simple weights verbatim; these
// govern the advanced-only signals layered on top. Personalization/session terms
// are rank-normalized to [0,1] across the candidate set before weighting, so a
// weight is a direct, interpretable cap on that signal's contribution.
type AdvancedWeights struct {
	CTR            float64 // smoothed, prior-centred click-through
	MeaningfulRate float64 // meaningful watches per click
	Affinity       float64 // neighbour-weighted personal affinity (normalized)
	Channel        float64 // direct channel affinity (normalized)
	Session        float64 // session co-search/co-watch intent (normalized)
	Language       float64 // language match bonus
	CreatorPenalty float64 // demotion per same-channel result beyond the 2nd
}

// DefaultAdvancedWeights are the shipped heuristic weights (documented starting
// values; tunable in one place, guarded by unit tests).
var DefaultAdvancedWeights = AdvancedWeights{
	CTR:            0.30,
	MeaningfulRate: 0.20,
	Affinity:       0.50,
	Channel:        0.30,
	Session:        0.25,
	Language:       0.10,
	CreatorPenalty: 0.15,
}

// CTR Beta-Binomial smoothing prior: (clicks+α)/(impressions+α+β). α=1, β=9 ⇒ a
// prior CTR of 0.1 with the weight of 10 pseudo-impressions. See
// SmoothedCTR/CTRPrior. The observed CTR is prior-centred (minus the prior) in the
// score so a doc with no engagement contributes exactly 0.
//
// NOTE on position debiasing: query_video_engagement aggregates impressions
// WITHOUT position resolution, so a true position-based propensity correction
// (dividing observed CTR by an examination prior per rank) is not available at
// this layer. We therefore use rank-independent Beta smoothing as a documented
// approximation; when per-position impression logging lands, divide clicks and
// impressions by the position examination prior before smoothing here.
const (
	ctrAlpha = 1.0
	ctrBeta  = 9.0
	// mwBeta smooths the meaningful-watch rate (meaningful / (clicks + mwBeta)).
	mwBeta = 5.0
)

// Features are the per-document signals the advanced ranker consumes. Text-match
// components mirror the simple ranker's inputs; the rest are advanced-only.
type Features struct {
	// Text-match components (stage-1 recall).
	TextRank   float64 // ts_rank_cd
	TrgmSim    float64 // trigram similarity(title, q)
	ExactFlags float64 // title 1.0 / channel 0.5 / tag 0.5
	Views      float64
	AgeDays    float64

	// Engagement (per query+video).
	Impressions       float64
	Clicks            float64
	MeaningfulWatches float64

	// Personalization (0 for anonymous / personalized=false).
	PersonalAffinity float64 // neighbour-weighted, pre-normalization
	ChannelAffinity  float64 // direct channel weight, pre-normalization
	SessionIntent    float64 // session co-search/co-watch, pre-normalization

	// Context.
	LanguageMatch bool
}

// Doc is one rerank candidate: its id, channel (for the creator cap/penalty), and
// features.
type Doc struct {
	VideoID   string
	ChannelID string // "" when unknown
	Features  Features
}

// Ranked is one scored result.
type Ranked struct {
	VideoID string
	Score   float64
}

// CTRPrior is the Beta prior click-through rate the smoothed CTR regresses to.
func CTRPrior() float64 { return ctrAlpha / (ctrAlpha + ctrBeta) }

// SmoothedCTR is (clicks+α)/(impressions+α+β): the Beta-Binomial posterior mean
// click-through, which regresses sparse evidence toward the prior.
func SmoothedCTR(clicks, impressions float64) float64 {
	return (clicks + ctrAlpha) / (impressions + ctrAlpha + ctrBeta)
}

// meaningfulRate is meaningful watches per click, smoothed so a click-less doc
// scores 0 rather than being undefined.
func meaningfulRate(meaningful, clicks float64) float64 {
	if meaningful <= 0 {
		return 0
	}
	return meaningful / (clicks + mwBeta)
}

// baseScore is the text/views/freshness sub-score — identical to SimpleScore so
// the advanced ranker degrades to the simple ordering when every advanced term is
// zero.
func baseScore(f Features) float64 {
	return SimpleScore(f.TextRank, f.TrgmSim, f.ExactFlags, f.Views, f.AgeDays)
}

// scoreWith computes the linear score for a doc whose personalization/session
// signals have ALREADY been normalized to [0,1] across the candidate set.
func scoreWith(f Features, w AdvancedWeights) float64 {
	s := baseScore(f)
	s += w.CTR * (SmoothedCTR(f.Clicks, f.Impressions) - CTRPrior())
	s += w.MeaningfulRate * meaningfulRate(f.MeaningfulWatches, f.Clicks)
	s += w.Affinity * f.PersonalAffinity
	s += w.Channel * f.ChannelAffinity
	s += w.Session * f.SessionIntent
	if f.LanguageMatch {
		s += w.Language
	}
	return s
}

// Scorer maps one doc's (already set-normalized) features to a score. The
// heuristic uses scoreWith; the learned ranker (internal/model) uses a
// leaves-loaded LightGBM model over ModelFeatureVector — both flow through
// RerankWith so the set-normalization, creator penalty, and deterministic
// ordering are shared.
type Scorer func(Features) float64

// Rerank scores and orders a candidate set with the advanced linear model. It
// rank-normalizes the unbounded personalization/session signals to [0,1] across
// the set (so a weight caps that signal's contribution), scores every doc, then
// applies the creator-repetition penalty: the 3rd and later results from one
// channel are demoted by CreatorPenalty per extra occurrence. Fully
// deterministic: ties break by descending score then video id.
func Rerank(docs []Doc, w AdvancedWeights) []Ranked {
	return RerankWith(docs, func(f Features) float64 { return scoreWith(f, w) }, w.CreatorPenalty)
}

// RerankWith is Rerank parameterized by the per-doc scorer, so the heuristic and
// the learned model share the identical set-normalization → score → creator
// penalty → deterministic sort pipeline. creatorPenalty demotes the 3rd+
// same-channel result (0 disables it).
func RerankWith(docs []Doc, scorer Scorer, creatorPenalty float64) []Ranked {
	if len(docs) == 0 {
		return nil
	}

	// Per-set maxima for the unbounded personalization signals.
	var maxAff, maxChan, maxSess float64
	for _, d := range docs {
		maxAff = math.Max(maxAff, d.Features.PersonalAffinity)
		maxChan = math.Max(maxChan, d.Features.ChannelAffinity)
		maxSess = math.Max(maxSess, d.Features.SessionIntent)
	}
	norm := func(v, max float64) float64 {
		if max > 0 {
			return v / max
		}
		return 0
	}

	type scored struct {
		doc     Doc
		score   float64
		channel string
	}
	items := make([]scored, 0, len(docs))
	for _, d := range docs {
		f := d.Features
		f.PersonalAffinity = norm(f.PersonalAffinity, maxAff)
		f.ChannelAffinity = norm(f.ChannelAffinity, maxChan)
		f.SessionIntent = norm(f.SessionIntent, maxSess)
		items = append(items, scored{doc: d, score: scorer(f), channel: d.ChannelID})
	}

	less := func(a, b scored) bool {
		if a.score != b.score {
			return a.score > b.score
		}
		return a.doc.VideoID < b.doc.VideoID
	}
	sort.SliceStable(items, func(i, j int) bool { return less(items[i], items[j]) })

	// Creator-repetition penalty: demote the 3rd+ same-channel result.
	if creatorPenalty > 0 {
		perChan := map[string]int{}
		for i := range items {
			ch := items[i].channel
			if ch == "" {
				continue
			}
			perChan[ch]++
			if perChan[ch] > 2 {
				items[i].score -= creatorPenalty * float64(perChan[ch]-2)
			}
		}
		sort.SliceStable(items, func(i, j int) bool { return less(items[i], items[j]) })
	}

	out := make([]Ranked, 0, len(items))
	for _, it := range items {
		out = append(out, Ranked{VideoID: it.doc.VideoID, Score: it.score})
	}
	return out
}

// ModelFeatureNames documents the fixed feature-vector order the learned ranker
// consumes. Python training MUST emit features in this exact order so the served
// model and the offline model agree. The vector is deliberately the set of
// features RECONSTRUCTABLE offline from query_video_engagement + the corpus:
// personalization (affinity/session) is online-only and stays heuristic in v1.
func ModelFeatureNames() []string {
	return []string{
		"text_rank", "trgm_sim", "exact_flags", "log_views",
		"age_days", "smoothed_ctr", "meaningful_rate", "language_match",
	}
}

// ModelFeatureVector builds the learned ranker's input vector from a doc's
// features, in ModelFeatureNames order.
func ModelFeatureVector(f Features) []float64 {
	lang := 0.0
	if f.LanguageMatch {
		lang = 1.0
	}
	return []float64{
		f.TextRank,
		f.TrgmSim,
		f.ExactFlags,
		math.Log(1 + f.Views),
		f.AgeDays,
		SmoothedCTR(f.Clicks, f.Impressions),
		meaningfulRate(f.MeaningfulWatches, f.Clicks),
		lang,
	}
}
