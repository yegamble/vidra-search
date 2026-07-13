// Package search implements simple- and advanced-mode search (§1.7) for
// GET /internal/v1/search. It returns ranked video IDs only — vidra-core
// hydrates them and applies per-viewer visibility. Simple mode scores + filters
// in one SQL round-trip (store.SearchSimple). Advanced mode does a two-stage
// funnel: SQL stage-1 recall (store.SearchAdvancedRecall, ≤500) then a Go stage-2
// rerank (internal/ranking) over text / engagement / personalization features,
// with the ranker chosen by experiment assignment (heuristic or learned model).
package search

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/vidra/vidra-search/internal/experiment"
	"github.com/vidra/vidra-search/internal/normalize"
	"github.com/vidra/vidra-search/internal/ranking"
	"github.com/vidra/vidra-search/internal/store/sqlcgen"
)

// ModelVersion identifies the W1 simple ranker.
const ModelVersion = "simple-v1"

// ExperimentKey is the experiment that routes advanced search to a ranker variant.
const ExperimentKey = "search_ranker"

const (
	defaultLimit = 20
	maxLimit     = 200
	// recallLimit bounds stage-1 advanced recall before the Go rerank.
	recallLimit = 500
)

// Querier is the store surface search reads.
type Querier interface {
	SearchSimple(ctx context.Context, arg sqlcgen.SearchSimpleParams) ([]sqlcgen.SearchSimpleRow, error)
	SearchAdvancedRecall(ctx context.Context, arg sqlcgen.SearchAdvancedRecallParams) ([]sqlcgen.SearchAdvancedRecallRow, error)
	NeighborAffinity(ctx context.Context, arg sqlcgen.NeighborAffinityParams) ([]sqlcgen.NeighborAffinityRow, error)
	UserChannelAffinity(ctx context.Context, userID uuid.UUID) ([]sqlcgen.UserChannelAffinityRow, error)
	NeighborScoresFromSeeds(ctx context.Context, arg sqlcgen.NeighborScoresFromSeedsParams) ([]sqlcgen.NeighborScoresFromSeedsRow, error)
}

// RankerProvider chooses the reranker for a request given the model version an
// experiment routes to. *model.Loader satisfies it; a nil provider falls back to
// the built-in heuristic.
type RankerProvider interface {
	RankerFor(wantVersion string) (ranking.Ranker, string)
}

// Experimenter assigns a subject to an experiment variant. *experiment.Registry
// satisfies it; nil disables experiments.
type Experimenter interface {
	Assign(key, subject string) (experiment.Assignment, bool)
}

// SessionVideoReader supplies the session's recent video ids (session-intent).
type SessionVideoReader interface {
	SessionVideos(ctx context.Context, sessionID string) []string
}

// Request is a parsed search request.
type Request struct {
	Query         string
	Limit         int
	Offset        int
	Tag           string
	Category      string
	Language      string
	HideSensitive bool
	Mode          string
	UserID        string
	SessionID     string
	Personalized  bool
}

// Hit is one ranked result: a video id and its score.
type Hit struct {
	VideoID string  `json:"video_id"`
	Score   float64 `json:"score"`
}

// Response is the search payload (§1.4).
type Response struct {
	Query        string                 `json:"query"`
	IDs          []Hit                  `json:"ids"`
	ModelVersion string                 `json:"model_version"`
	Experiment   *experiment.Assignment `json:"experiment,omitempty"`
}

// Service runs simple- and advanced-mode search.
type Service struct {
	q       Querier
	ranker  RankerProvider
	exp     Experimenter
	session SessionVideoReader
	logger  *slog.Logger
}

// NewService builds the search service. ranker, exp, and session may be nil:
// advanced mode then uses the built-in heuristic with no experiment routing and
// no session-intent signal.
func NewService(q Querier, ranker RankerProvider, exp Experimenter, session SessionVideoReader, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{q: q, ranker: ranker, exp: exp, session: session, logger: logger}
}

// Search runs the query and returns ranked ids. An empty (or whitespace-only)
// query returns no results rather than the whole corpus. mode=advanced runs the
// two-stage funnel; any other mode runs simple-mode SQL scoring.
func (s *Service) Search(ctx context.Context, req Request) (Response, error) {
	normalized := normalize.Normalize(req.Query)
	if normalized == "" {
		return Response{Query: req.Query, IDs: []Hit{}, ModelVersion: ModelVersion}, nil
	}
	if req.Mode == "advanced" {
		return s.searchAdvanced(ctx, req, normalized)
	}
	return s.searchSimple(ctx, req, normalized)
}

// searchSimple is the single-round-trip SQL-scored path (§1.7 simple).
func (s *Service) searchSimple(ctx context.Context, req Request, normalized string) (Response, error) {
	resp := Response{Query: req.Query, IDs: []Hit{}, ModelVersion: ModelVersion}
	limit := clampLimit(req.Limit)
	offset := req.Offset
	if offset < 0 {
		offset = 0
	}
	rows, err := s.q.SearchSimple(ctx, sqlcgen.SearchSimpleParams{
		Query:         normalized,
		HideSensitive: req.HideSensitive,
		Tag:           optStr(req.Tag),
		Category:      optStr(req.Category),
		Language:      optStr(req.Language),
		Off:           int32(offset),
		Lim:           int32(limit),
	})
	if err != nil {
		return Response{}, err
	}
	hits := make([]Hit, 0, len(rows))
	for _, r := range rows {
		hits = append(hits, Hit{VideoID: r.VideoID.String(), Score: r.Score})
	}
	resp.IDs = hits
	return resp, nil
}

func clampLimit(v int) int {
	if v <= 0 {
		return defaultLimit
	}
	if v > maxLimit {
		return maxLimit
	}
	return v
}

// optStr maps an empty filter to a nil (SQL NULL) optional parameter.
func optStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
