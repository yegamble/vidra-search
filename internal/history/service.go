// Package history serves the user search-history + privacy endpoints (§1.4): read
// a user's history, clear it, delete a single entry, and fully purge a user.
//
// CLEARING DELETES. It used to anonymize — NULL the user_id and keep the row —
// and that was not a milder deletion, it was the opposite of one: an attributed
// row carries no subject_id, so every k-anonymity floor's
// COALESCE(subject_id, session_id) fallback turned one account's N sessions into
// N distinct anonymous "subjects", and the clear PUBLISHED associations and
// queries the floor had been suppressing. queries/history.sql carries the
// argument and the measured numbers. Deleting is also what makes the class of
// bug impossible rather than merely fixed: removing rows can only lower a
// distinct-subject count, never raise one.
//
// Multi-statement operations run in a single transaction so a partial clear can
// never leave a half-deleted footprint, and the ephemeral Redis session-recency
// lists are dropped after it commits.
package history

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/vidra/vidra-search/internal/paging"
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

// SessionRecency is the slice of the Redis cache a deletion needs: the ephemeral
// per-session recent-query/recent-video lists (sess:q:/sess:v:, 2h TTL) that
// autosuggest and session-intent ranking read back. An interface, not the
// concrete cache, so the history service stays testable and a nil cache is a
// supported configuration.
type SessionRecency interface {
	DropSessionRecency(ctx context.Context, sessionIDs []string) error
}

// Service implements the history + privacy operations.
type Service struct {
	store *store.Store
	redis SessionRecency
}

// NewService builds the history service. redis may be nil; the session lists
// then simply expire on their own within their TTL.
func NewService(st *store.Store, redis SessionRecency) *Service {
	return &Service{store: st, redis: redis}
}

// List returns a user's non-hidden history, most-recent first.
func (s *Service) List(ctx context.Context, userID uuid.UUID, limit, offset int) (ListResponse, error) {
	limit = paging.Limit(limit, defaultLimit, maxLimit)
	offset = paging.Offset(offset)
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

// ClearAll deletes a user's search history and their rows in the raw
// query_log/behavior_events ledgers, in one transaction, then drops the ephemeral
// session-recency lists those rows named.
func (s *Service) ClearAll(ctx context.Context, userID uuid.UUID) error {
	sessions, err := s.deleteSearchFootprint(ctx, userID, false)
	if err != nil {
		return err
	}
	s.dropSessionRecency(ctx, sessions)
	return nil
}

// DeleteEntry removes a single history entry (by its normalized query) AND the
// account's ledger rows carrying that query. "Forget I searched X" has to mean
// the same kind of thing clear-all means: leaving those rows behind would keep
// the user counted toward X's instance-wide distinct-subject floor after they
// asked to be forgotten for X. If they search it again it is recreated fresh.
func (s *Service) DeleteEntry(ctx context.Context, userID uuid.UUID, normalizedQuery string) error {
	return s.inTx(ctx, func(q *sqlcgen.Queries) error {
		if err := q.DeleteUserSearchHistoryEntry(ctx, sqlcgen.DeleteUserSearchHistoryEntryParams{
			UserID: userID, NormalizedQuery: normalizedQuery,
		}); err != nil {
			return err
		}
		if _, err := q.DeleteQueryLogForUserQuery(ctx, sqlcgen.DeleteQueryLogForUserQueryParams{
			UserID: pgconv.UUID(userID), NormalizedQuery: normalizedQuery,
		}); err != nil {
			return err
		}
		_, err := q.DeleteBehaviorEventsForUserQuery(ctx, sqlcgen.DeleteBehaviorEventsForUserQueryParams{
			UserID: pgconv.UUID(userID), NormalizedQuery: &normalizedQuery,
		})
		return err
	})
}

// PurgeUser fully removes a user's footprint: search history, watch projection,
// and every ledger row naming them, in one transaction.
func (s *Service) PurgeUser(ctx context.Context, userID uuid.UUID) error {
	sessions, err := s.deleteSearchFootprint(ctx, userID, true)
	if err != nil {
		return err
	}
	s.dropSessionRecency(ctx, sessions)
	return nil
}

// deleteSearchFootprint is the shared body of ClearAll and PurgeUser: the search
// scope, optionally plus the watch projection. It returns the session ids the
// deleted rows carried, read INSIDE the transaction and before the deletes —
// afterwards there is nothing left to read them from.
func (s *Service) deleteSearchFootprint(ctx context.Context, userID uuid.UUID, withWatch bool) ([]string, error) {
	var sessions []string
	err := s.inTx(ctx, func(q *sqlcgen.Queries) error {
		var err error
		if sessions, err = q.ListUserSessionIDs(ctx, pgconv.UUID(userID)); err != nil {
			return err
		}
		if err := q.DeleteUserSearchHistory(ctx, userID); err != nil {
			return err
		}
		if withWatch {
			if err := q.PurgeUserWatchProjection(ctx, userID); err != nil {
				return err
			}
		}
		if _, err := q.DeleteQueryLogForUser(ctx, pgconv.UUID(userID)); err != nil {
			return err
		}
		_, err = q.DeleteBehaviorEventsForUser(ctx, pgconv.UUID(userID))
		return err
	})
	if err != nil {
		return nil, err
	}
	return sessions, nil
}

// dropSessionRecency clears the Redis lists after the transaction commits.
// Best-effort by design: the rows are already gone, the lists carry a 2h TTL, and
// a Redis blip must not fail a deletion the database has committed.
func (s *Service) dropSessionRecency(ctx context.Context, sessions []string) {
	if s.redis == nil || len(sessions) == 0 {
		return
	}
	_ = s.redis.DropSessionRecency(ctx, sessions)
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
