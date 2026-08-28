// Package search implements simple- and advanced-mode search (§1.7) for
// GET /internal/v1/search. It returns ranked video IDs only — vidra-core
// hydrates them and applies per-viewer visibility. Simple mode scores + filters
// in SQL (store.SearchSimple), plus a second round-trip for the exact hit count
// (store.SearchSimpleCount) unless the caller skips it. Advanced mode does a
// two-stage funnel: SQL stage-1 recall (store.SearchAdvancedRecall, capped at a
// window that widens with the requested page) then a Go stage-2 rerank
// (internal/ranking) over text / engagement / personalization features, with the
// ranker chosen by experiment assignment (heuristic or learned model). See the
// Response doc comment for how the two modes report their totals.
package search

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/vidra/vidra-search/internal/experiment"
	"github.com/vidra/vidra-search/internal/normalize"
	"github.com/vidra/vidra-search/internal/paging"
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
	// recallLimit is the base stage-1 advanced recall block: the window a
	// first-page advanced request recalls before the Go rerank.
	recallLimit = 500
	// maxRecallLimit is the hard ceiling on the advanced recall window. A request
	// paging past it is served an empty page with total_is_lower_bound set, which
	// is the response saying "we stopped looking" rather than "you reached the
	// end" — see the Response doc comment.
	maxRecallLimit = 4 * recallLimit
)

// Querier is the store surface search reads.
type Querier interface {
	SearchSimple(ctx context.Context, arg sqlcgen.SearchSimpleParams) ([]sqlcgen.SearchSimpleRow, error)
	SearchSimpleCount(ctx context.Context, arg sqlcgen.SearchSimpleCountParams) (int64, error)
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
	License       string
	HideSensitive bool
	Mode          string
	UserID        string
	SessionID     string
	Personalized  bool
	// SkipCount drops the simple-mode COUNT(*) round-trip for callers that do not
	// need a total (Response.Total is then nil). It is a no-op in advanced mode,
	// whose total is a by-product of the recall it already runs. The default —
	// false — computes the count, because the point of the field is that the UI
	// gets a total.
	SkipCount bool
}

// Hit is one ranked result: a video id and its score.
type Hit struct {
	VideoID string  `json:"video_id"`
	Score   float64 `json:"score"`
}

// Response is the search payload (§1.4).
//
// Paging contract — Total, TotalIsLowerBound and HasMore together let a client
// paging forward tell "you have seen every match" apart from "this service
// stopped looking", which a bare page of ids cannot express:
//
//   - Total counts the documents matching the query AND the request's filters
//     (eligibility, hide_sensitive, tag, category, language, license), ignoring
//     limit/offset. It is nil only when the caller asked to skip the count; nil
//     means "not computed", never "zero".
//   - In SIMPLE mode Total is EXACT. It is a COUNT(*) over the very same FROM +
//     WHERE the page query pages over, so `offset + len(IDs) == *Total` is a
//     reliable end-of-results test.
//   - In ADVANCED mode Total is the size of the stage-1 recall set the Go
//     reranker actually saw — the number of ranked results this service can
//     serve, not the number of documents in the corpus that match. That recall
//     is capped (maxRecallLimit), so when the cap is reached Total is a LOWER
//     BOUND: more matching documents exist, they were never ranked.
//   - TotalIsLowerBound is true in exactly that case and false everywhere else,
//     including every simple-mode response and every advanced response whose
//     recall came back under the cap (there Total is exact too).
//   - HasMore is true when a forward page from this service would still return
//     at least one result. It is exact, and computed independently of Total, so
//     it stays correct under skip_count. It is false — and paging must stop —
//     once the recall ceiling is reached, which is precisely when
//     TotalIsLowerBound is the field that explains why.
//
// So: HasMore drives "fetch another page"; TotalIsLowerBound qualifies how the
// total should be rendered ("2,000 results" vs "top 2,000 results").
type Response struct {
	Query        string                 `json:"query"`
	IDs          []Hit                  `json:"ids"`
	ModelVersion string                 `json:"model_version"`
	Experiment   *experiment.Assignment `json:"experiment,omitempty"`
	// Total is the hit count described above; nil when skip_count was requested.
	Total *int `json:"total,omitempty"`
	// TotalIsLowerBound marks Total as "at least this many" rather than exact.
	TotalIsLowerBound bool `json:"total_is_lower_bound"`
	// HasMore reports whether a further page would return results.
	HasMore bool `json:"has_more"`
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
		// An empty query matches nothing, and that zero is exact — say so rather
		// than leaving the client to guess whether the total was skipped.
		return Response{Query: req.Query, IDs: []Hit{}, ModelVersion: ModelVersion, Total: intPtr(0)}, nil
	}
	if req.Mode == "advanced" {
		return s.searchAdvanced(ctx, req, normalized)
	}
	return s.searchSimple(ctx, req, normalized)
}

// searchSimple is the SQL-scored path (§1.7 simple). Simple mode pushes
// limit/offset into SQL, so it has no recall cliff: its total is exact and
// HasMore is decided by over-fetching a single sentinel row, which keeps HasMore
// correct even when the caller skips the count.
func (s *Service) searchSimple(ctx context.Context, req Request, normalized string) (Response, error) {
	resp := Response{Query: req.Query, IDs: []Hit{}, ModelVersion: ModelVersion}
	limit := paging.Limit(req.Limit, defaultLimit, maxLimit)
	offset := paging.Offset(req.Offset)
	// Ask for one row past the page. Its presence — not an inference from the
	// count — is what makes HasMore exact.
	rows, err := s.q.SearchSimple(ctx, sqlcgen.SearchSimpleParams{
		Query:         normalized,
		HideSensitive: req.HideSensitive,
		Tag:           optStr(req.Tag),
		Category:      optStr(req.Category),
		Language:      optStr(req.Language),
		License:       optStr(req.License),
		Off:           int32(offset),
		Lim:           int32(limit + 1),
	})
	if err != nil {
		return Response{}, err
	}
	if len(rows) > limit {
		resp.HasMore = true
		rows = rows[:limit]
	}
	hits := make([]Hit, 0, len(rows))
	for _, r := range rows {
		hits = append(hits, Hit{VideoID: r.VideoID.String(), Score: r.Score})
	}
	resp.IDs = hits

	if !req.SkipCount {
		// SearchSimpleCount shares SearchSimple's FROM + WHERE verbatim, so this
		// total is exact for the page query above — never a lower bound.
		total, err := s.q.SearchSimpleCount(ctx, sqlcgen.SearchSimpleCountParams{
			Query:         normalized,
			HideSensitive: req.HideSensitive,
			Tag:           optStr(req.Tag),
			Category:      optStr(req.Category),
			Language:      optStr(req.Language),
			License:       optStr(req.License),
		})
		if err != nil {
			return Response{}, err
		}
		resp.Total = intPtr(int(total))
	}
	return resp, nil
}

// intPtr returns a pointer to v (Response.Total distinguishes 0 from "not computed").
func intPtr(v int) *int { return &v }

// optStr maps an empty filter to a nil (SQL NULL) optional parameter.
func optStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
