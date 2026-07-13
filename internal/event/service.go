package event

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vidra/vidra-search/internal/index"
	"github.com/vidra/vidra-search/internal/normalize"
	"github.com/vidra/vidra-search/internal/pgconv"
	"github.com/vidra/vidra-search/internal/store"
	"github.com/vidra/vidra-search/internal/store/sqlcgen"
)

// MaxBatch is the largest number of events accepted in one POST /events call.
const MaxBatch = 500

// Metrics is the subset of telemetry used by the ingest path.
type Metrics interface {
	ObserveEvent(typ, outcome string)
	ObserveEventLag(seconds float64)
}

// RedisSink receives the ephemeral, non-transactional side effects of behavioral
// events (session recency + global trending). They are flushed only AFTER the
// database transaction commits, and are best-effort: a failure is logged, never
// surfaced to the caller. cache.Cache satisfies it; tests may pass nil.
type RedisSink interface {
	PushSessionQuery(ctx context.Context, sessionID, normalizedQuery string) error
	PushSessionVideo(ctx context.Context, sessionID, videoID string) error
	TrendBump(ctx context.Context, domain, item, subject string, halfLifeSeconds float64, capWindow time.Duration) error
}

// Config carries the intake-time tuning knobs (trending decay + per-user cap
// window, watch-affinity half-life). Zero values fall back to sane defaults.
type Config struct {
	TrendingHalfLifeSeconds float64
	TrendCapWindow          time.Duration
	WatchHalfLifeHours      float64
}

func (c Config) withDefaults() Config {
	if c.TrendingHalfLifeSeconds <= 0 {
		c.TrendingHalfLifeSeconds = 6 * 3600
	}
	if c.TrendCapWindow <= 0 {
		c.TrendCapWindow = time.Hour
	}
	if c.WatchHalfLifeHours <= 0 {
		c.WatchHalfLifeHours = 720
	}
	return c
}

// watch-projection weight deltas (§1.5).
const (
	weightPlayStarted     = 0.3
	weightMeaningfulWatch = 1.0 // applied by the engagement worker on derivation
	weightCompleted       = 1.5
)

// Service applies event batches to the search corpus.
type Service struct {
	store   *store.Store
	metrics Metrics
	logger  *slog.Logger
	cfg     Config
	redis   RedisSink
}

// NewService builds the ingest service. metrics/redis may be nil; cfg's zero
// values are defaulted.
func NewService(st *store.Store, metrics Metrics, logger *slog.Logger, cfg Config, redis RedisSink) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{store: st, metrics: metrics, logger: logger, cfg: cfg.withDefaults(), redis: redis}
}

func (s *Service) observe(typ, outcome string) {
	if s.metrics != nil {
		s.metrics.ObserveEvent(metricType(typ), outcome)
	}
}

// effectKind tags a deferred Redis side effect.
type effectKind int

const (
	effSessionQuery effectKind = iota
	effSessionVideo
	effTrend
)

// redisEffect is one deferred Redis operation, executed after the DB commit.
type redisEffect struct {
	kind      effectKind
	sessionID string
	value     string
	domain    string
	item      string
	subject   string
}

// Ingest deduplicates and applies a batch of events in a single transaction.
// Each event runs inside its own savepoint so one malformed or failing event is
// isolated (recorded in failed[]) without poisoning the rest of the batch. A
// redelivered event conflicts on the inbox and is counted a duplicate, its side
// effects skipped — so replaying an identical batch is a no-op. The ephemeral
// Redis side effects of successfully-committed events are flushed afterward.
func (s *Service) Ingest(ctx context.Context, events []Envelope) (Result, error) {
	res := Result{Failed: []Failure{}}
	if len(events) == 0 {
		return res, nil
	}

	tx, err := s.store.Begin(ctx)
	if err != nil {
		return res, fmt.Errorf("event: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	base := s.store.Queries()
	var effects []redisEffect
	for _, ev := range events {
		outcome, evEffects, dup, ferr := s.applyOne(ctx, tx, base, ev)
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
			s.observeLag(ev.OccurredAt)
			effects = append(effects, evEffects...)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return Result{Failed: []Failure{}}, fmt.Errorf("event: commit: %w", err)
	}
	s.flush(ctx, effects)
	return res, nil
}

func (s *Service) observeLag(occurredAt time.Time) {
	if s.metrics == nil || occurredAt.IsZero() {
		return
	}
	if lag := time.Since(occurredAt).Seconds(); lag >= 0 {
		s.metrics.ObserveEventLag(lag)
	}
}

// flush runs the deferred Redis side effects best-effort.
func (s *Service) flush(ctx context.Context, effects []redisEffect) {
	if s.redis == nil || len(effects) == 0 {
		return
	}
	for _, e := range effects {
		var err error
		switch e.kind {
		case effSessionQuery:
			err = s.redis.PushSessionQuery(ctx, e.sessionID, e.value)
		case effSessionVideo:
			err = s.redis.PushSessionVideo(ctx, e.sessionID, e.value)
		case effTrend:
			err = s.redis.TrendBump(ctx, e.domain, e.item, e.subject, s.cfg.TrendingHalfLifeSeconds, s.cfg.TrendCapWindow)
		}
		if err != nil {
			s.logger.WarnContext(ctx, "event: redis side effect failed", "kind", int(e.kind), "error", err)
		}
	}
}

// applyOne runs a single event inside a savepoint. It returns
// (outcome, effects, dup, err): dup=true when already seen; err!=nil when
// application failed (savepoint rolled back, retriable). Effects are returned only
// for a freshly-committed event.
func (s *Service) applyOne(ctx context.Context, tx pgx.Tx, base *sqlcgen.Queries, ev Envelope) (string, []redisEffect, bool, error) {
	sub, err := tx.Begin(ctx) // savepoint
	if err != nil {
		return "", nil, false, err
	}
	q := base.WithTx(sub)

	if _, ierr := q.InsertInboxEvent(ctx, sqlcgen.InsertInboxEventParams{EventID: ev.EventID, Type: ev.Type}); ierr != nil {
		if errors.Is(ierr, pgx.ErrNoRows) {
			_ = sub.Rollback(ctx)
			return "", nil, true, nil // duplicate
		}
		_ = sub.Rollback(ctx)
		return "", nil, false, ierr
	}

	outcome, effects, aerr := s.dispatch(ctx, q, ev)
	if aerr != nil {
		_ = sub.Rollback(ctx)
		return "", nil, false, aerr
	}
	if cerr := sub.Commit(ctx); cerr != nil {
		return "", nil, false, cerr
	}
	return outcome, effects, false, nil
}

// dispatch applies one already-deduped event and reports the accepted outcome
// ("accepted" for domain effects, "counted" for behavioral, "ignored" for
// unknown/forward-compatible types) plus any deferred Redis effects.
func (s *Service) dispatch(ctx context.Context, q *sqlcgen.Queries, ev Envelope) (string, []redisEffect, error) {
	switch ev.Type {
	case TypeVideoUpsert:
		var p videoUpsertPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return "", nil, err
		}
		return "accepted", nil, applyVideoUpsert(ctx, q, p.Video, nil)

	case TypeVideoSuppress:
		var p videoSuppressPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return "", nil, err
		}
		reason := p.Reason
		return "accepted", nil, q.SuppressDocument(ctx, sqlcgen.SuppressDocumentParams{
			VideoID: p.VideoID, Reason: &reason,
		})

	case TypeVideoStats:
		var p videoStatsPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return "", nil, err
		}
		return "accepted", nil, q.UpdateDocumentStats(ctx, sqlcgen.UpdateDocumentStatsParams{
			VideoID: p.VideoID, Views: p.Views, Likes: p.Likes,
		})

	case TypeChannelUpsert:
		var p channelUpsertPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return "", nil, err
		}
		handle := p.Channel.Handle
		name := p.Channel.DisplayName
		return "accepted", nil, q.UpdateChannelDocuments(ctx, sqlcgen.UpdateChannelDocumentsParams{
			ChannelID:     pgconv.UUID(p.Channel.ID),
			ChannelHandle: &handle,
			ChannelName:   &name,
			OwnerID:       pgconv.UUIDPtr(p.Channel.OwnerID),
		})

	case TypeChannelDelete:
		var p channelDeletePayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return "", nil, err
		}
		reason := "channel_deleted"
		return "accepted", nil, q.SuppressChannelDocuments(ctx, sqlcgen.SuppressChannelDocumentsParams{
			ChannelID: pgconv.UUID(p.ChannelID), Reason: &reason,
		})

	case TypeUserSuppress:
		var p userSuppressPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return "", nil, err
		}
		reason := "owner_unlisted"
		if p.Unlisted {
			return "accepted", nil, q.SuppressOwnerDocuments(ctx, sqlcgen.SuppressOwnerDocumentsParams{
				OwnerID: pgconv.UUID(p.UserID), Reason: &reason,
			})
		}
		return "accepted", nil, q.RestoreOwnerDocuments(ctx, sqlcgen.RestoreOwnerDocumentsParams{
			OwnerID: pgconv.UUID(p.UserID), Reason: &reason,
		})

	case TypeConfigUpdated:
		var p configUpdatedPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return "", nil, err
		}
		for k, v := range p.Settings {
			if err := q.UpsertServiceConfig(ctx, sqlcgen.UpsertServiceConfigParams{
				Key: k, Value: stringifyConfigValue(v),
			}); err != nil {
				return "", nil, err
			}
		}
		return "accepted", nil, nil

	case TypeReconcileBegin:
		return "accepted", nil, nil

	case TypeReconcilePage:
		var p reconcilePagePayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return "", nil, err
		}
		runID := p.RunID
		for i := range p.Videos {
			if err := applyVideoUpsert(ctx, q, p.Videos[i], &runID); err != nil {
				return "", nil, err
			}
		}
		return "accepted", nil, nil

	case TypeReconcileEnd:
		var p reconcileEndPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return "", nil, err
		}
		return "accepted", nil, q.SuppressReconcileOrphans(ctx, pgconv.UUID(p.RunID))

	case TypeUserHistoryDel:
		var p userHistoryDeletedPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return "", nil, err
		}
		return "accepted", nil, applyHistoryDeleted(ctx, q, p)
	}

	if isBehavioral(ev.Type) {
		effects, err := s.applyBehavioral(ctx, q, ev)
		return "counted", effects, err
	}
	// Unknown type — accepted and ignored for forward compatibility.
	return "ignored", nil, nil
}

// applyBehavioral records one behavioral event to behavior_events and applies its
// durable side effects (query_log, history, projection) plus deferred Redis
// effects. The behavior_events row is written for EVERY behavioral type; the
// durable personal projections are gated by the history-collection rule.
func (s *Service) applyBehavioral(ctx context.Context, q *sqlcgen.Queries, ev Envelope) ([]redisEffect, error) {
	switch ev.Type {
	case TypeSearchSubmitted:
		var p searchSubmittedPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return nil, err
		}
		nq := normalize.Normalize(p.Query)
		display := strings.TrimSpace(p.Query)
		if err := s.insertBehavior(ctx, q, ev, behaviorFields{
			userID: p.UserID, sessionID: p.SessionID, normalizedQuery: nilIfEmpty(nq),
		}); err != nil {
			return nil, err
		}
		if nq != "" {
			if err := q.AppendQueryLog(ctx, sqlcgen.AppendQueryLogParams{
				EventID: pgconv.UUID(ev.EventID), NormalizedQuery: nq, DisplayQuery: display,
				UserID: pgconv.UUIDPtr(p.UserID), SessionID: p.SessionID,
				ResultsCount: p.ResultsCount, SubmittedAt: eventTime(ev),
			}); err != nil {
				return nil, err
			}
		}
		if p.AllowHistory && p.UserID != nil && nq != "" {
			if err := q.UpsertUserSearchHistory(ctx, sqlcgen.UpsertUserSearchHistoryParams{
				UserID: *p.UserID, NormalizedQuery: nq, DisplayQuery: display, LastUsedAt: eventTime(ev),
			}); err != nil {
				return nil, err
			}
		}
		var effects []redisEffect
		if sid := derefStr(p.SessionID); sid != "" && nq != "" {
			effects = append(effects, redisEffect{kind: effSessionQuery, sessionID: sid, value: nq})
		}
		if nq != "" {
			effects = append(effects, redisEffect{kind: effTrend, domain: "q", item: nq, subject: subjectOf(p.UserID, p.SessionID)})
		}
		return effects, nil

	case TypeVideoPlayStarted:
		var p playStartedPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return nil, err
		}
		nq := ""
		if p.Query != nil {
			nq = normalize.Normalize(*p.Query)
		}
		if err := s.insertBehavior(ctx, q, ev, behaviorFields{
			userID: p.UserID, sessionID: p.SessionID, normalizedQuery: nilIfEmpty(nq), videoID: &p.VideoID,
		}); err != nil {
			return nil, err
		}
		if p.AllowHistory && p.UserID != nil {
			if err := s.upsertProjection(ctx, q, *p.UserID, p.VideoID, weightPlayStarted); err != nil {
				return nil, err
			}
		}
		var effects []redisEffect
		if sid := derefStr(p.SessionID); sid != "" {
			effects = append(effects, redisEffect{kind: effSessionVideo, sessionID: sid, value: p.VideoID.String()})
		}
		effects = append(effects, redisEffect{kind: effTrend, domain: "v", item: p.VideoID.String(), subject: subjectOf(p.UserID, p.SessionID)})
		return effects, nil

	case TypeVideoCompleted:
		var p videoCompletedPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return nil, err
		}
		if err := s.insertBehavior(ctx, q, ev, behaviorFields{
			userID: p.UserID, sessionID: p.SessionID, videoID: &p.VideoID,
		}); err != nil {
			return nil, err
		}
		if p.AllowHistory && p.UserID != nil {
			if err := s.upsertProjection(ctx, q, *p.UserID, p.VideoID, weightCompleted); err != nil {
				return nil, err
			}
		}
		return nil, nil

	case TypeVideoWatchProgress:
		var p watchProgressPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return nil, err
		}
		return nil, s.insertBehavior(ctx, q, ev, behaviorFields{
			userID: p.UserID, sessionID: p.SessionID, videoID: &p.VideoID,
		})

	case TypeSearchResultClicked:
		var p resultClickedPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return nil, err
		}
		nq := normalize.Normalize(p.Query)
		return nil, s.insertBehavior(ctx, q, ev, behaviorFields{
			userID: p.UserID, sessionID: p.SessionID, normalizedQuery: nilIfEmpty(nq),
			videoID: &p.VideoID, position: p.Position, modelVersion: p.ModelVersion,
		})

	case TypeVideoImpression:
		var p impressionPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return nil, err
		}
		nq := ""
		if p.Query != nil {
			nq = normalize.Normalize(*p.Query)
		}
		return nil, s.insertBehavior(ctx, q, ev, behaviorFields{
			userID: p.UserID, sessionID: p.SessionID, normalizedQuery: nilIfEmpty(nq),
			videoID: &p.VideoID, position: p.Position, modelVersion: p.ModelVersion,
		})

	case TypeSearchSuggShown, TypeSearchSuggSelected:
		var p suggestionEventPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return nil, err
		}
		return nil, s.insertBehavior(ctx, q, ev, behaviorFields{
			userID: p.UserID, sessionID: p.SessionID, position: p.Position,
		})
	}
	return nil, nil
}

// behaviorFields are the per-type mapped columns for a behavior_events row.
type behaviorFields struct {
	userID          *uuid.UUID
	sessionID       *string
	normalizedQuery *string
	videoID         *uuid.UUID
	position        *int32
	modelVersion    *string
}

// insertBehavior writes the behavior_events row, storing the full original
// payload as props for the workers.
func (s *Service) insertBehavior(ctx context.Context, q *sqlcgen.Queries, ev Envelope, f behaviorFields) error {
	return q.InsertBehaviorEvent(ctx, sqlcgen.InsertBehaviorEventParams{
		EventID:         ev.EventID,
		Type:            ev.Type,
		UserID:          pgconv.UUIDPtr(f.userID),
		SessionID:       f.sessionID,
		NormalizedQuery: f.normalizedQuery,
		VideoID:         pgconv.UUIDPtr(f.videoID),
		Position:        f.position,
		ModelVersion:    f.modelVersion,
		OccurredAt:      eventTime(ev),
		Props:           []byte(ev.Payload),
	})
}

func (s *Service) upsertProjection(ctx context.Context, q *sqlcgen.Queries, userID, videoID uuid.UUID, delta float32) error {
	return q.UpsertWatchProjection(ctx, sqlcgen.UpsertWatchProjectionParams{
		UserID: userID, VideoID: videoID, Delta: delta, HalfLifeHours: s.cfg.WatchHalfLifeHours,
	})
}

// applyHistoryDeleted purges the personal projections for a user.history_deleted
// event. watch → watch projection; search → search history + anonymized logs;
// all → both.
func applyHistoryDeleted(ctx context.Context, q *sqlcgen.Queries, p userHistoryDeletedPayload) error {
	if p.Scope == "watch" || p.Scope == "all" {
		if err := q.PurgeUserWatchProjection(ctx, p.UserID); err != nil {
			return err
		}
	}
	if p.Scope == "search" || p.Scope == "all" {
		if err := q.DeleteUserSearchHistory(ctx, p.UserID); err != nil {
			return err
		}
		if err := q.AnonymizeQueryLogUser(ctx, pgconv.UUID(p.UserID)); err != nil {
			return err
		}
		if err := q.AnonymizeBehaviorEventsUser(ctx, pgconv.UUID(p.UserID)); err != nil {
			return err
		}
	}
	return nil
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

// eventTime returns the event's occurred_at, defaulting to now if unset so the
// NOT NULL timestamp columns are always populated.
func eventTime(ev Envelope) time.Time {
	if ev.OccurredAt.IsZero() {
		return time.Now().UTC()
	}
	return ev.OccurredAt
}

// subjectOf returns the id used for per-user trending caps + distinct-user HLL:
// the user id when signed in, else the session id.
func subjectOf(userID *uuid.UUID, sessionID *string) string {
	if userID != nil {
		return userID.String()
	}
	return derefStr(sessionID)
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// derefTimeNow returns *t, or the current time when t is nil.
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
