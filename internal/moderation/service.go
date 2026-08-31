// Package moderation implements the operator-facing suggestion-ban surface: the
// write path for search.query_aggregates.banned, which suggestions.sql reads and
// rollups.sql preserves but which nothing else ever set. Without it, removing an
// abusive string from instance-wide autosuggest means hand-written SQL against
// an undocumented column.
//
// This service moderates GLOBAL, aggregated query strings only. It stores no
// per-viewer state and takes no per-viewer input: a ban applies to the whole
// instance, exactly like the distinct-user threshold it complements. Per-viewer
// visibility stays in vidra-core (docs/privacy.md).
package moderation

import (
	"context"
	"time"

	"github.com/vidra/vidra-search/internal/normalize"
	"github.com/vidra/vidra-search/internal/paging"
	"github.com/vidra/vidra-search/internal/store/sqlcgen"
)

const (
	defaultLimit = 20
	maxLimit     = 100
)

// Querier is the store surface the moderation service writes and reads.
type Querier interface {
	BanSuggestion(ctx context.Context, normalizedQuery string) error
	UnbanSuggestion(ctx context.Context, normalizedQuery string) error
	ListBannedSuggestions(ctx context.Context, arg sqlcgen.ListBannedSuggestionsParams) ([]sqlcgen.ListBannedSuggestionsRow, error)
}

// ErrEmptyQuery reports a ban/unban target that normalizes away to nothing.
type ErrEmptyQuery struct{}

func (ErrEmptyQuery) Error() string { return "moderation: query is empty after normalization" }

// BanResponse confirms which aggregate key was actually banned. The service
// normalizes its input, so the caller cannot assume the key it sent is the key
// that moved — echoing it back is what makes a later unban land on the same row.
type BanResponse struct {
	NormalizedQuery string `json:"normalized_query"`
	Banned          bool   `json:"banned"`
}

// BannedEntry is one row of the reviewable ban list. It carries the counts a
// second operator needs to judge whether a ban placed by someone else was right.
type BannedEntry struct {
	NormalizedQuery string    `json:"normalized_query"`
	Query           string    `json:"query"`
	TotalCount      int64     `json:"total_count"`
	DistinctUsers   int32     `json:"distinct_users"`
	FirstSeen       time.Time `json:"first_seen"`
	LastSeen        time.Time `json:"last_seen"`
}

// ListResponse is the paginated ban list (shaped like history.ListResponse).
type ListResponse struct {
	Entries []BannedEntry `json:"entries"`
	Limit   int           `json:"limit"`
	Offset  int           `json:"offset"`
}

// Service implements the suggestion-ban operations.
type Service struct {
	q Querier
}

// NewService builds the moderation service.
func NewService(q Querier) *Service { return &Service{q: q} }

// Ban suppresses a query from the global suggestion stream. The input is put
// through the same normalizer the ingest and suggest paths use, so an operator
// who bans the display form ("Buy Cheap Followers") hits the aggregate row the
// rollup writes ("buy cheap followers"). Normalize is idempotent, so passing an
// already-normalized key is a no-op.
func (s *Service) Ban(ctx context.Context, query string) (BanResponse, error) {
	nq := normalize.Normalize(query)
	if nq == "" {
		return BanResponse{}, ErrEmptyQuery{}
	}
	if err := s.q.BanSuggestion(ctx, nq); err != nil {
		return BanResponse{}, err
	}
	return BanResponse{NormalizedQuery: nq, Banned: true}, nil
}

// Unban lifts a ban. It does not restore suggestibility — see the comment on
// UnbanSuggestion in queries/moderation.sql: the rollup re-earns that from real
// distinct-user counts, so an unban can never promote a query that never cleared
// the aggregation threshold.
func (s *Service) Unban(ctx context.Context, query string) error {
	nq := normalize.Normalize(query)
	if nq == "" {
		return ErrEmptyQuery{}
	}
	return s.q.UnbanSuggestion(ctx, nq)
}

// List returns a page of currently-banned queries so a ban can be reviewed and
// reversed by someone who did not place it.
func (s *Service) List(ctx context.Context, limit, offset int) (ListResponse, error) {
	limit = paging.Limit(limit, defaultLimit, maxLimit)
	offset = paging.Offset(offset)
	rows, err := s.q.ListBannedSuggestions(ctx, sqlcgen.ListBannedSuggestionsParams{
		Lim: int32(limit), Off: int32(offset),
	})
	if err != nil {
		return ListResponse{}, err
	}
	entries := make([]BannedEntry, 0, len(rows))
	for _, r := range rows {
		display := r.DisplayQuery
		if display == "" {
			display = r.NormalizedQuery
		}
		entries = append(entries, BannedEntry{
			NormalizedQuery: r.NormalizedQuery,
			Query:           display,
			TotalCount:      r.TotalCount,
			DistinctUsers:   r.DistinctUsers,
			FirstSeen:       r.FirstSeen,
			LastSeen:        r.LastSeen,
		})
	}
	return ListResponse{Entries: entries, Limit: limit, Offset: offset}, nil
}
