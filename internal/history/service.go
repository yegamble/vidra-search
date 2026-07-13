// Package history serves the user search-history + privacy endpoints (§1.4): read
// a user's history, clear it (anonymizing the raw logs), delete a single entry,
// and fully purge a user. Clearing/purging NULLs the user_id in query_log and
// behavior_events rather than deleting them, so global aggregates stay intact
// while the data no longer references the user. Multi-statement operations run in
// a single transaction so a partial purge can never leave dangling references.
package history

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/vidra/vidra-search/internal/pgconv"
	"github.com/vidra/vidra-search/internal/store"
	"github.com/vidra/vidra-search/internal/store/sqlcgen"
)

const (
	defaultLimit = 20
	maxLimit     = 100
)

// Entry is one history row surfaced to the caller (display query + metadata).
type Entry struct {
	Query           string    `json:"query"`
	NormalizedQuery string    `json:"normalized_query"`
	LastUsedAt      time.Time `json:"last_used_at"`
	UseCount        int32     `json:"use_count"`
}

// ListResponse is the paginated history payload (§1.4).
type ListResponse struct {
	Entries []Entry `json:"entries"`
	Limit   int     `json:"limit"`
	Offset  int     `json:"offset"`
}

// Service implements the history + privacy operations.
type Service struct {
	store *store.Store
}

// NewService builds the history service.
func NewService(st *store.Store) *Service { return &Service{store: st} }

// List returns a user's non-hidden history, most-recent first.
func (s *Service) List(ctx context.Context, userID uuid.UUID, limit, offset int) (ListResponse, error) {
	limit = clamp(limit)
	if offset < 0 {
		offset = 0
	}
	rows, err := s.store.Queries().ListUserSearchHistory(ctx, sqlcgen.ListUserSearchHistoryParams{
		UserID: userID, Lim: int32(limit), Off: int32(offset),
	})
	if err != nil {
		return ListResponse{}, err
	}
	entries := make([]Entry, 0, len(rows))
	for _, r := range rows {
		entries = append(entries, Entry{
			Query: r.DisplayQuery, NormalizedQuery: r.NormalizedQuery,
			LastUsedAt: r.LastUsedAt, UseCount: r.UseCount,
		})
	}
	return ListResponse{Entries: entries, Limit: limit, Offset: offset}, nil
}

// ClearAll deletes a user's search history and anonymizes their user_id in the raw
// query_log/behavior_events ledgers, in one transaction.
func (s *Service) ClearAll(ctx context.Context, userID uuid.UUID) error {
	return s.inTx(ctx, func(q *sqlcgen.Queries) error {
		if err := q.DeleteUserSearchHistory(ctx, userID); err != nil {
			return err
		}
		if err := q.AnonymizeQueryLogUser(ctx, pgconv.UUID(userID)); err != nil {
			return err
		}
		return q.AnonymizeBehaviorEventsUser(ctx, pgconv.UUID(userID))
	})
}

// DeleteEntry removes a single history entry (by its normalized query). If the
// user searches it again it is re-created fresh, so a hard delete is correct.
func (s *Service) DeleteEntry(ctx context.Context, userID uuid.UUID, normalizedQuery string) error {
	return s.store.Queries().DeleteUserSearchHistoryEntry(ctx, sqlcgen.DeleteUserSearchHistoryEntryParams{
		UserID: userID, NormalizedQuery: normalizedQuery,
	})
}

// PurgeUser fully removes a user's footprint: search history, watch projection,
// and anonymized user_id everywhere, in one transaction.
func (s *Service) PurgeUser(ctx context.Context, userID uuid.UUID) error {
	return s.inTx(ctx, func(q *sqlcgen.Queries) error {
		if err := q.DeleteUserSearchHistory(ctx, userID); err != nil {
			return err
		}
		if err := q.PurgeUserWatchProjection(ctx, userID); err != nil {
			return err
		}
		if err := q.AnonymizeQueryLogUser(ctx, pgconv.UUID(userID)); err != nil {
			return err
		}
		return q.AnonymizeBehaviorEventsUser(ctx, pgconv.UUID(userID))
	})
}

func (s *Service) inTx(ctx context.Context, fn func(*sqlcgen.Queries) error) error {
	tx, err := s.store.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(s.store.Queries().WithTx(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func clamp(v int) int {
	if v <= 0 {
		return defaultLimit
	}
	if v > maxLimit {
		return maxLimit
	}
	return v
}
