package event

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vidra/vidra-search/internal/index"
	"github.com/vidra/vidra-search/internal/pgconv"
	"github.com/vidra/vidra-search/internal/store"
	"github.com/vidra/vidra-search/internal/store/sqlcgen"
)

// MaxBatch is the largest number of events accepted in one POST /events call.
const MaxBatch = 500

// Metrics is the subset of telemetry used by the ingest path.
type Metrics interface {
	ObserveEvent(typ, outcome string)
}

// Service applies event batches to the search corpus.
type Service struct {
	store   *store.Store
	metrics Metrics
	logger  *slog.Logger
}

// NewService builds the ingest service. metrics/logger may be nil.
func NewService(st *store.Store, metrics Metrics, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{store: st, metrics: metrics, logger: logger}
}

func (s *Service) observe(typ, outcome string) {
	if s.metrics != nil {
		s.metrics.ObserveEvent(metricType(typ), outcome)
	}
}

// Ingest deduplicates and applies a batch of events in a single transaction.
// Each event runs inside its own savepoint so one malformed or failing event is
// isolated (recorded in failed[]) without poisoning the rest of the batch. A
// redelivered event conflicts on the inbox and is counted a duplicate, its side
// effects skipped — so replaying an identical batch is a no-op.
func (s *Service) Ingest(ctx context.Context, events []Envelope) (Result, error) {
	var res Result
	if len(events) == 0 {
		return res, nil
	}

	tx, err := s.store.Begin(ctx)
	if err != nil {
		return res, fmt.Errorf("event: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	base := s.store.Queries()
	for _, ev := range events {
		outcome, dup, ferr := s.applyOne(ctx, tx, base, ev)
		switch {
		case ferr != nil:
			res.Failed = append(res.Failed, Failure{
				EventID: ev.EventID.String(), Code: "apply_failed", Message: ferr.Error(),
			})
			s.observe(ev.Type, "failed")
		case dup:
			res.Duplicates++
			s.observe(ev.Type, "duplicate")
		default:
			res.Accepted++
			s.observe(ev.Type, outcome)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return Result{}, fmt.Errorf("event: commit: %w", err)
	}
	return res, nil
}

// applyOne runs a single event inside a savepoint. It returns (outcome, dup,
// err): dup=true when the event was already seen; err!=nil when application
// failed (the savepoint is rolled back so the inbox row is not persisted, and
// the event can be safely retried later).
func (s *Service) applyOne(ctx context.Context, tx pgx.Tx, base *sqlcgen.Queries, ev Envelope) (outcome string, dup bool, err error) {
	sub, err := tx.Begin(ctx) // savepoint
	if err != nil {
		return "", false, err
	}
	q := base.WithTx(sub)

	if _, ierr := q.InsertInboxEvent(ctx, sqlcgen.InsertInboxEventParams{EventID: ev.EventID, Type: ev.Type}); ierr != nil {
		if errors.Is(ierr, pgx.ErrNoRows) {
			_ = sub.Rollback(ctx)
			return "", true, nil // duplicate
		}
		_ = sub.Rollback(ctx)
		return "", false, ierr
	}

	outcome, aerr := s.dispatch(ctx, q, ev)
	if aerr != nil {
		_ = sub.Rollback(ctx)
		return "", false, aerr
	}
	if cerr := sub.Commit(ctx); cerr != nil {
		return "", false, cerr
	}
	return outcome, false, nil
}

// dispatch applies one already-deduped event and reports the accepted outcome
// ("accepted" for domain effects, "counted" for behavioral, "ignored" for
// unknown/forward-compatible types).
func (s *Service) dispatch(ctx context.Context, q *sqlcgen.Queries, ev Envelope) (string, error) {
	switch ev.Type {
	case TypeVideoUpsert:
		var p videoUpsertPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return "", err
		}
		return "accepted", applyVideoUpsert(ctx, q, p.Video, nil)

	case TypeVideoSuppress:
		var p videoSuppressPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return "", err
		}
		reason := p.Reason
		return "accepted", q.SuppressDocument(ctx, sqlcgen.SuppressDocumentParams{
			VideoID: p.VideoID, Reason: &reason,
		})

	case TypeVideoStats:
		var p videoStatsPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return "", err
		}
		return "accepted", q.UpdateDocumentStats(ctx, sqlcgen.UpdateDocumentStatsParams{
			VideoID: p.VideoID, Views: p.Views, Likes: p.Likes,
		})

	case TypeChannelUpsert:
		var p channelUpsertPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return "", err
		}
		handle := p.Channel.Handle
		name := p.Channel.DisplayName
		return "accepted", q.UpdateChannelDocuments(ctx, sqlcgen.UpdateChannelDocumentsParams{
			ChannelID:     pgconv.UUID(p.Channel.ID),
			ChannelHandle: &handle,
			ChannelName:   &name,
			OwnerID:       pgconv.UUIDPtr(p.Channel.OwnerID),
		})

	case TypeChannelDelete:
		var p channelDeletePayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return "", err
		}
		reason := "channel_deleted"
		return "accepted", q.SuppressChannelDocuments(ctx, sqlcgen.SuppressChannelDocumentsParams{
			ChannelID: pgconv.UUID(p.ChannelID), Reason: &reason,
		})

	case TypeUserSuppress:
		var p userSuppressPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return "", err
		}
		reason := "owner_unlisted"
		if p.Unlisted {
			return "accepted", q.SuppressOwnerDocuments(ctx, sqlcgen.SuppressOwnerDocumentsParams{
				OwnerID: pgconv.UUID(p.UserID), Reason: &reason,
			})
		}
		return "accepted", q.RestoreOwnerDocuments(ctx, sqlcgen.RestoreOwnerDocumentsParams{
			OwnerID: pgconv.UUID(p.UserID), Reason: &reason,
		})

	case TypeConfigUpdated:
		var p configUpdatedPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return "", err
		}
		for k, v := range p.Settings {
			if err := q.UpsertServiceConfig(ctx, sqlcgen.UpsertServiceConfigParams{
				Key: k, Value: stringifyConfigValue(v),
			}); err != nil {
				return "", err
			}
		}
		return "accepted", nil

	case TypeReconcileBegin:
		// No persistent state needed: reconcile.end carries the run_id used to
		// suppress orphans, and reconcile.page stamps each document.
		return "accepted", nil

	case TypeReconcilePage:
		var p reconcilePagePayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return "", err
		}
		runID := p.RunID
		for i := range p.Videos {
			if err := applyVideoUpsert(ctx, q, p.Videos[i], &runID); err != nil {
				return "", err
			}
		}
		return "accepted", nil

	case TypeReconcileEnd:
		var p reconcileEndPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return "", err
		}
		return "accepted", q.SuppressReconcileOrphans(ctx, pgconv.UUID(p.RunID))

	case TypeUserHistoryDel:
		// W1: history projections are not built yet, so this is a no-op. W2 purges
		// user_search_history / user_watch_projection here.
		return "accepted", nil
	}

	if isBehavioral(ev.Type) {
		// W1: accepted and counted for observability, but not persisted. W2 writes
		// these to behavior_events for aggregation.
		return "counted", nil
	}
	// Unknown type — accepted and ignored for forward compatibility.
	return "ignored", nil
}

// applyVideoUpsert maps a video document to an UpsertDocument. reconcileRunID is
// non-nil only for reconcile.page (it stamps the row); a normal upsert passes
// nil so the existing stamp is preserved by the query's COALESCE.
func applyVideoUpsert(ctx context.Context, q *sqlcgen.Queries, v VideoDoc, reconcileRunID *uuid.UUID) error {
	eligible, reason := index.Eligible(v.Privacy, v.State)
	var suppressed *string
	if !eligible {
		r := reason
		suppressed = &r
	}
	kind := v.Kind
	if kind == "" {
		kind = "local"
	}
	tags := v.Tags
	if tags == nil {
		tags = []string{}
	}
	// source_updated_at is NOT NULL; prefer the payload's updated_at, else now.
	var src = v.UpdatedAt
	if src == nil {
		src = v.CreatedAt
	}
	params := sqlcgen.UpsertDocumentParams{
		VideoID:          v.ID,
		Kind:             kind,
		ChannelID:        pgconv.UUIDPtr(v.ChannelID),
		ChannelHandle:    v.ChannelHandle,
		ChannelName:      v.ChannelName,
		OwnerID:          pgconv.UUIDPtr(v.OwnerID),
		Title:            v.Title,
		Description:      v.Description,
		Tags:             tags,
		Category:         v.Category,
		Language:         v.Language,
		DurationSeconds:  v.DurationSeconds,
		IsSensitive:      v.IsSensitive,
		Eligible:         eligible,
		SuppressedReason: suppressed,
		Views:            v.Views,
		Likes:            v.Likes,
		PublishedAt:      pgconv.TimePtr(v.PublishedAt),
		SourceUpdatedAt:  derefTimeNow(src),
		ReconcileRunID:   pgconv.UUIDPtr(reconcileRunID),
	}
	return q.UpsertDocument(ctx, params)
}

// derefTimeNow returns *t, or the current time when t is nil, so source_updated_at
// (NOT NULL) is always populated even if the event omits both timestamps.
func derefTimeNow(t *time.Time) time.Time {
	if t == nil {
		return time.Now().UTC()
	}
	return *t
}

// stringifyConfigValue renders a JSON config value as the TEXT stored in
// service_config. Numbers without a fractional part render as integers.
func stringifyConfigValue(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", t)
	}
}
