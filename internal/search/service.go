// Package search implements simple-mode hybrid search (§1.7) for
// GET /internal/v1/search. It returns ranked video IDs only — vidra-core
// hydrates them and applies per-viewer visibility. Scoring and filtering happen
// in one SQL round-trip (store.SearchSimple); this layer normalizes the query,
// clamps paging, and shapes the response.
package search

import (
	"context"

	"github.com/vidra/vidra-search/internal/normalize"
	"github.com/vidra/vidra-search/internal/store/sqlcgen"
)

// ModelVersion identifies the W1 simple ranker.
const ModelVersion = "simple-v1"

const (
	defaultLimit = 20
	maxLimit     = 200
)

// Querier is the store surface search reads.
type Querier interface {
	SearchSimple(ctx context.Context, arg sqlcgen.SearchSimpleParams) ([]sqlcgen.SearchSimpleRow, error)
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
}

// Hit is one ranked result: a video id and its score.
type Hit struct {
	VideoID string  `json:"video_id"`
	Score   float64 `json:"score"`
}

// Response is the search payload (§1.4).
type Response struct {
	Query        string `json:"query"`
	IDs          []Hit  `json:"ids"`
	ModelVersion string `json:"model_version"`
}

// Service runs simple-mode search.
type Service struct {
	q Querier
}

// NewService builds the search service.
func NewService(q Querier) *Service {
	return &Service{q: q}
}

// Search runs the query and returns ranked ids. An empty (or whitespace-only)
// query returns no results rather than the whole corpus.
func (s *Service) Search(ctx context.Context, req Request) (Response, error) {
	normalized := normalize.Normalize(req.Query)
	resp := Response{Query: req.Query, IDs: []Hit{}, ModelVersion: ModelVersion}
	if normalized == "" {
		return resp, nil
	}
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
