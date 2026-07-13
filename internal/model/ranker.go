package model

import "github.com/vidra/vidra-search/internal/ranking"

// Ranker is the shared reranker contract (ranking.Ranker): the heuristic (always
// available) and the learned model (used only when a valid artifact is loaded AND
// an experiment routes to it) both implement it.
type Ranker = ranking.Ranker

// HeuristicVersion is the version string reported when the hand-tuned linear
// ranker is used.
const HeuristicVersion = "heuristic-v1"

// Heuristic is the always-available hand-tuned linear ranker.
type Heuristic struct {
	weights ranking.AdvancedWeights
	version string
}

// NewHeuristic builds the default heuristic ranker.
func NewHeuristic() *Heuristic {
	return &Heuristic{weights: ranking.DefaultAdvancedWeights, version: HeuristicVersion}
}

// Rerank scores the candidates with the linear model.
func (h *Heuristic) Rerank(docs []ranking.Doc) []ranking.Ranked {
	return ranking.Rerank(docs, h.weights)
}

// Version reports the heuristic version.
func (h *Heuristic) Version() string { return h.version }

// Learned wraps a loaded LightGBM model as a Ranker. It shares the creator
// penalty and set-normalization pipeline with the heuristic (RerankWith); its
// per-doc score is the model's prediction over the documented feature vector.
type Learned struct {
	model   *LeavesModel
	version string
	penalty float64
}

// NewLearned builds a learned ranker from a loaded model and the version string
// the experiment routes to. penalty is the creator-repetition demotion applied
// on top of the model score.
func NewLearned(m *LeavesModel, version string, penalty float64) *Learned {
	return &Learned{model: m, version: version, penalty: penalty}
}

// Rerank scores candidates with the learned model over ModelFeatureVector.
func (l *Learned) Rerank(docs []ranking.Doc) []ranking.Ranked {
	return ranking.RerankWith(docs, func(f ranking.Features) float64 {
		return l.model.Predict(ranking.ModelFeatureVector(f))
	}, l.penalty)
}

// Version reports the learned model's version.
func (l *Learned) Version() string { return l.version }
