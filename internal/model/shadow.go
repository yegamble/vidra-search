package model

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vidra/vidra-search/internal/pgconv"
	"github.com/vidra/vidra-search/internal/ranking"
	"github.com/vidra/vidra-search/internal/store/sqlcgen"
)

// ndcgK is the cutoff for the shadow-evaluation NDCG/MRR metrics.
const ndcgK = 10

// ShadowMetrics is the shadow evaluator's telemetry seam (nil-safe).
type ShadowMetrics interface {
	SetShadowMetric(version, metric string, value float64)
}

// ShadowQuerier is the store surface the evaluator reads/writes.
type ShadowQuerier interface {
	ListShadowModels(ctx context.Context, kind string) ([]sqlcgen.SearchModel, error)
	ShadowImpressions(ctx context.Context, days int32) ([]sqlcgen.ShadowImpressionsRow, error)
	SearchAdvancedRecall(ctx context.Context, arg sqlcgen.SearchAdvancedRecallParams) ([]sqlcgen.SearchAdvancedRecallRow, error)
	UpdateModelMetrics(ctx context.Context, arg sqlcgen.UpdateModelMetricsParams) error
}

// ShadowEvaluator scores shadow ranker models offline against logged impressions
// (§1.9). It replays the last N days of impressions + their click/meaningful
// labels, re-ranks each impression list with the shadow model AND the heuristic,
// and compares NDCG@10 / MRR@10 to the production ordering actually served. The
// results are written to the model's metrics JSONB and to Prometheus gauges; it
// NEVER activates a model (activation is a manual step) and NEVER touches serving.
type ShadowEvaluator struct {
	q       ShadowQuerier
	loader  *Loader
	metrics ShadowMetrics
	logger  *slog.Logger
	days    int
}

// NewShadowEvaluator builds an evaluator over the last `days` of impressions.
func NewShadowEvaluator(q ShadowQuerier, loader *Loader, metrics ShadowMetrics, logger *slog.Logger, days int) *ShadowEvaluator {
	if logger == nil {
		logger = slog.Default()
	}
	if days < 1 {
		days = 14
	}
	return &ShadowEvaluator{q: q, loader: loader, metrics: metrics, logger: logger, days: days}
}

// impItem is one impressed video with its served position and graded relevance.
type impItem struct {
	videoID  uuid.UUID
	position int32
	rel      float64
}

// impGroup is one impression list: the videos shown for a (session, query).
type impGroup struct {
	query string
	items []impItem
}

// Report is the metrics blob written to a model's metrics JSONB.
type Report struct {
	Groups         int       `json:"groups"`
	ShadowNDCG     float64   `json:"ndcg@10"`
	ShadowMRR10    float64   `json:"mrr@10"`
	ProductionNDCG float64   `json:"production_ndcg@10"`
	HeuristicNDCG  float64   `json:"heuristic_ndcg@10"`
	VsProduction   float64   `json:"vs_production"` // shadow − production NDCG@10
	VsHeuristic    float64   `json:"vs_heuristic"`  // shadow − heuristic NDCG@10
	WindowDays     int       `json:"window_days"`
	EvaluatedAt    time.Time `json:"evaluated_at"`
	FeatureVersion string    `json:"feature_version"`
}

// Run evaluates every shadow ranker model against the recent impression log. It
// is safe to run with no shadow models or no impressions (a no-op / zeroed
// report). Returns an error only on a store failure.
func (e *ShadowEvaluator) Run(ctx context.Context) error {
	shadows, err := e.q.ListShadowModels(ctx, "ranker")
	if err != nil {
		return err
	}
	if len(shadows) == 0 {
		return nil
	}

	imps, err := e.q.ShadowImpressions(ctx, int32(e.days))
	if err != nil {
		return err
	}
	groups := groupImpressions(imps)

	recallCache := map[string]map[uuid.UUID]ranking.Features{}
	heuristic := e.loader.Heuristic()

	for _, m := range shadows {
		learned, lerr := e.loader.LoadModel(m)
		if lerr != nil {
			e.logger.WarnContext(ctx, "shadow_eval: shadow artifact failed to load; skipping",
				"version", m.Version, "error", lerr)
			if e.metrics != nil {
				e.metrics.SetShadowMetric(m.Version, "load_error", 1)
			}
			continue
		}

		var n int
		var sumProd, sumShadow, sumHeur, sumShadowMRR float64
		for _, g := range groups {
			if !usableGroup(g) {
				continue
			}
			feats := e.recallFor(ctx, g.query, recallCache)
			docs := docsForGroup(g, feats)

			prodRels := relsInServedOrder(g)
			shadowRels := relsInRankedOrder(learned.Rerank(docs), g)
			heurRels := relsInRankedOrder(heuristic.Rerank(docs), g)

			sumProd += NDCGAt(prodRels, ndcgK)
			sumShadow += NDCGAt(shadowRels, ndcgK)
			sumHeur += NDCGAt(heurRels, ndcgK)
			sumShadowMRR += MRRAt(shadowRels, ndcgK)
			n++
		}

		rep := Report{Groups: n, WindowDays: e.days, EvaluatedAt: time.Now().UTC(), FeatureVersion: "v1"}
		if n > 0 {
			rep.ShadowNDCG = round(sumShadow / float64(n))
			rep.ShadowMRR10 = round(sumShadowMRR / float64(n))
			rep.ProductionNDCG = round(sumProd / float64(n))
			rep.HeuristicNDCG = round(sumHeur / float64(n))
			rep.VsProduction = round((sumShadow - sumProd) / float64(n))
			rep.VsHeuristic = round((sumShadow - sumHeur) / float64(n))
		}

		blob, _ := json.Marshal(rep)
		if err := e.q.UpdateModelMetrics(ctx, sqlcgen.UpdateModelMetricsParams{ID: m.ID, Metrics: blob}); err != nil {
			return err
		}
		if e.metrics != nil {
			e.metrics.SetShadowMetric(m.Version, "ndcg@10", rep.ShadowNDCG)
			e.metrics.SetShadowMetric(m.Version, "mrr@10", rep.ShadowMRR10)
			e.metrics.SetShadowMetric(m.Version, "vs_production", rep.VsProduction)
			e.metrics.SetShadowMetric(m.Version, "vs_heuristic", rep.VsHeuristic)
		}
		e.logger.InfoContext(ctx, "shadow_eval: scored model",
			"version", m.Version, "groups", n, "ndcg@10", rep.ShadowNDCG,
			"vs_production", rep.VsProduction, "vs_heuristic", rep.VsHeuristic)
	}
	return nil
}

// groupImpressions folds the flat impression rows into per-(session,query) lists.
func groupImpressions(rows []sqlcgen.ShadowImpressionsRow) []impGroup {
	type key struct {
		session string
		query   string
	}
	order := []key{}
	byKey := map[key]*impGroup{}
	for _, r := range rows {
		vid, ok := pgconv.UUIDValue(r.VideoID)
		if !ok || r.NormalizedQuery == nil {
			continue
		}
		k := key{session: deref(r.SessionID), query: *r.NormalizedQuery}
		g, ok := byKey[k]
		if !ok {
			g = &impGroup{query: k.query}
			byKey[k] = g
			order = append(order, k)
		}
		g.items = append(g.items, impItem{videoID: vid, position: r.Position, rel: labelOf(r)})
	}
	out := make([]impGroup, 0, len(order))
	for _, k := range order {
		out = append(out, *byKey[k])
	}
	return out
}

// labelOf derives the graded relevance: meaningful-watch 2, click 1, else 0.
func labelOf(r sqlcgen.ShadowImpressionsRow) float64 {
	switch {
	case r.Meaningful:
		return 2
	case r.Clicked:
		return 1
	default:
		return 0
	}
}

// usableGroup keeps only groups with ≥2 items and ≥1 positive label — the rest
// carry no ranking signal.
func usableGroup(g impGroup) bool {
	if len(g.items) < 2 {
		return false
	}
	for _, it := range g.items {
		if it.rel >= 1 {
			return true
		}
	}
	return false
}

// recallFor returns (and caches) the per-video advanced features for a query.
func (e *ShadowEvaluator) recallFor(ctx context.Context, query string, cache map[string]map[uuid.UUID]ranking.Features) map[uuid.UUID]ranking.Features {
	if f, ok := cache[query]; ok {
		return f
	}
	rows, err := e.q.SearchAdvancedRecall(ctx, sqlcgen.SearchAdvancedRecallParams{Query: query, HideSensitive: false, Lim: 500})
	m := map[uuid.UUID]ranking.Features{}
	if err == nil {
		now := time.Now()
		for _, r := range rows {
			m[r.VideoID] = featuresFromRecall(r, now)
		}
	}
	cache[query] = m
	return m
}

// docsForGroup builds ranking.Doc for a group's impressed videos, using recall
// features when available (zero-value features otherwise).
func docsForGroup(g impGroup, feats map[uuid.UUID]ranking.Features) []ranking.Doc {
	docs := make([]ranking.Doc, 0, len(g.items))
	for _, it := range g.items {
		docs = append(docs, ranking.Doc{VideoID: it.videoID.String(), Features: feats[it.videoID]})
	}
	return docs
}

// relsInServedOrder returns the labels in the order the videos were shown
// (position ascending — the rows are already position-sorted per group).
func relsInServedOrder(g impGroup) []float64 {
	out := make([]float64, len(g.items))
	for i, it := range g.items {
		out[i] = it.rel
	}
	return out
}

// relsInRankedOrder returns the labels in the order a ranker placed the videos.
func relsInRankedOrder(ranked []ranking.Ranked, g impGroup) []float64 {
	rel := make(map[string]float64, len(g.items))
	for _, it := range g.items {
		rel[it.videoID.String()] = it.rel
	}
	out := make([]float64, 0, len(ranked))
	for _, r := range ranked {
		out = append(out, rel[r.VideoID])
	}
	return out
}

// featuresFromRecall maps an advanced-recall row to ranking.Features (mirrors the
// search service's extraction; personalization is out of scope offline).
func featuresFromRecall(r sqlcgen.SearchAdvancedRecallRow, now time.Time) ranking.Features {
	return ranking.Features{
		TextRank:          r.TsRank,
		TrgmSim:           r.TrgmSim,
		ExactFlags:        r.ExactFlags,
		Views:             float64(r.Views),
		AgeDays:           recallAgeDays(r.PublishedAt, r.SourceUpdatedAt, now),
		Impressions:       float64(r.Impressions),
		Clicks:            float64(r.Clicks),
		MeaningfulWatches: float64(r.MeaningfulWatches),
	}
}

func recallAgeDays(publishedAt pgtype.Timestamptz, sourceUpdatedAt, now time.Time) float64 {
	t := sourceUpdatedAt
	if publishedAt.Valid {
		t = publishedAt.Time
	}
	d := now.Sub(t).Hours() / 24
	if d < 0 {
		return 0
	}
	return d
}

func round(v float64) float64 {
	return float64(int64(v*10000+0.5)) / 10000
}
