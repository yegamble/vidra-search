// Package worker runs vidra-search's background rollup loops (§1.9): the
// aggregate/engagement rollups that fold the raw event ledgers into suggestion +
// engagement aggregates, the sessionizer that derives reformulation/abandonment,
// the trending sweeper that decays and gates the Redis trend lists, and the
// retention + reconcile-guard housekeepers. Each is a ticker loop wired in
// cmd/api (single-binary pattern, mirroring vidra-core); cadences are env-tunable.
// The rollups are cursor-based and advance their bookmark in the same transaction
// as the writes they cover, so a crash resumes rather than reprocesses.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vidra/vidra-search/internal/cache"
	"github.com/vidra/vidra-search/internal/pgconv"
	"github.com/vidra/vidra-search/internal/store"
	"github.com/vidra/vidra-search/internal/store/sqlcgen"
	"github.com/vidra/vidra-search/internal/trending"
)

// derivedNamespace is the fixed UUID namespace for deterministic derived-event
// ids (matches the DeriveMeaningfulWatch SQL). The event NAME disambiguates types.
var derivedNamespace = uuid.MustParse("6ba7b814-9dad-11d1-80b4-00c04fd430c8")

// weightMeaningfulWatch is the watch-projection delta applied when a meaningful
// watch is derived (§1.5).
const weightMeaningfulWatch = 1.0

// Metrics is the worker-facing telemetry surface (nil-safe via the Runner).
type Metrics interface {
	ObserveRollup(worker string, seconds float64)
	IncWorkerError(worker string)
	IncTrendingRejection(domain, reason string)
	SetReconcileAge(seconds float64)
}

// Config holds worker cadences and tuning. Zero values are defaulted.
type Config struct {
	AggregatesInterval     time.Duration
	EngagementInterval     time.Duration
	SessionizerInterval    time.Duration
	TrendingInterval       time.Duration
	CovisInterval          time.Duration
	RetentionInterval      time.Duration
	ReconcileGuardInterval time.Duration
	JobTimeout             time.Duration

	// Co-visitation tuning (§1.9 covis_rollup).
	CovisWindowSeconds float64 // max in-session gap for a co-occurrence pair
	CovisLambda        float64 // cosine shrinkage λ (algorithms report ≈10)
	CovisTopM          int     // neighbors kept per item

	// Policy knobs (env fallbacks; service_config overlay wins at runtime).
	MinQueryUserCount       int
	RetentionDays           int
	QueryHalfLifeSeconds    float64 // decayed_freq half-life for suggestions
	TrendingHalfLifeSeconds float64 // trend ZSET decay half-life
	WatchHalfLifeHours      float64
	TrendCapWindow          time.Duration
	MeaningfulWatchSeconds  int
	MeaningfulWatchPct      int

	// Trending sweeper.
	TrendPruneFloor float64
	TrendWindowDays int
	WilsonFloor     float64
	TrendingTopK    int
	TrendingListTTL time.Duration

	// Sessionizer windows.
	ReformulationGap time.Duration
	AbandonWindow    time.Duration

	// Retention.
	InboxRetentionDays int
	ProjectionFloor    float64
	ReconcileMaxAge    time.Duration
}

func (c Config) withDefaults() Config {
	set := func(d *time.Duration, def time.Duration) {
		if *d <= 0 {
			*d = def
		}
	}
	set(&c.AggregatesInterval, time.Minute)
	set(&c.EngagementInterval, 5*time.Minute)
	set(&c.SessionizerInterval, 5*time.Minute)
	set(&c.TrendingInterval, time.Minute)
	set(&c.CovisInterval, 15*time.Minute)
	set(&c.RetentionInterval, 24*time.Hour)
	set(&c.ReconcileGuardInterval, 10*time.Minute)
	set(&c.JobTimeout, 2*time.Minute)
	set(&c.TrendCapWindow, time.Hour)
	set(&c.ReformulationGap, 60*time.Second)
	set(&c.AbandonWindow, 5*time.Minute)
	set(&c.ReconcileMaxAge, 48*time.Hour)
	set(&c.TrendingListTTL, 5*time.Minute)
	if c.MinQueryUserCount < 1 {
		c.MinQueryUserCount = 3
	}
	if c.RetentionDays < 1 {
		c.RetentionDays = 90
	}
	if c.QueryHalfLifeSeconds <= 0 {
		c.QueryHalfLifeSeconds = 7 * 24 * 3600 // 7-day QAC half-life (algorithms report 3-14d)
	}
	if c.TrendingHalfLifeSeconds <= 0 {
		c.TrendingHalfLifeSeconds = 6 * 3600
	}
	if c.WatchHalfLifeHours <= 0 {
		c.WatchHalfLifeHours = 720
	}
	if c.MeaningfulWatchSeconds < 1 {
		c.MeaningfulWatchSeconds = 30
	}
	if c.MeaningfulWatchPct < 1 {
		c.MeaningfulWatchPct = 30
	}
	if c.TrendPruneFloor <= 0 {
		c.TrendPruneFloor = 0.01
	}
	if c.TrendWindowDays < 1 {
		c.TrendWindowDays = 2
	}
	if c.WilsonFloor <= 0 {
		c.WilsonFloor = 0.10
	}
	if c.TrendingTopK < 1 {
		c.TrendingTopK = 50
	}
	if c.InboxRetentionDays < 1 {
		c.InboxRetentionDays = 7
	}
	if c.ProjectionFloor <= 0 {
		c.ProjectionFloor = 0.05
	}
	if c.CovisWindowSeconds <= 0 {
		c.CovisWindowSeconds = 3600 // 1h in-session co-occurrence window
	}
	if c.CovisLambda <= 0 {
		c.CovisLambda = 10 // shrinkage λ (algorithms report)
	}
	if c.CovisTopM < 1 {
		c.CovisTopM = 100 // top neighbors per item
	}
	return c
}

// PeriodicJob is an externally-supplied ticker loop (model_loader, experiment
// refresh, shadow_eval) run alongside the built-in rollups, so cmd/api can wire
// jobs that depend on packages the worker does not import.
type PeriodicJob struct {
	Name     string
	Interval time.Duration
	Run      func(context.Context) error
}

// Runner owns the store, cache, config, and telemetry the loops share.
type Runner struct {
	store   *store.Store
	cache   *cache.Cache
	cfg     Config
	metrics Metrics
	logger  *slog.Logger
	extra   []PeriodicJob
}

// AddJob registers an external periodic job. Call before Start. The job is also
// reachable via RunOnce(name) for deterministic test drives.
func (r *Runner) AddJob(j PeriodicJob) {
	if j.Interval <= 0 {
		j.Interval = time.Minute
	}
	r.extra = append(r.extra, j)
}

// NewRunner builds a Runner. cache/metrics may be nil (trending-dependent work is
// then skipped); cfg is defaulted.
func NewRunner(st *store.Store, c *cache.Cache, cfg Config, metrics Metrics, logger *slog.Logger) *Runner {
	if logger == nil {
		logger = slog.Default()
	}
	return &Runner{store: st, cache: c, cfg: cfg.withDefaults(), metrics: metrics, logger: logger}
}

// Start launches every loop in its own goroutine and returns immediately; the
// loops stop when ctx is cancelled.
func (r *Runner) Start(ctx context.Context) {
	go r.runLoop(ctx, "aggregates_rollup", r.cfg.AggregatesInterval, r.aggregatesRollup)
	go r.runLoop(ctx, "engagement_rollup", r.cfg.EngagementInterval, r.engagementRollup)
	go r.runLoop(ctx, "sessionizer", r.cfg.SessionizerInterval, r.sessionizer)
	go r.runLoop(ctx, "covis_rollup", r.cfg.CovisInterval, r.covisRollup)
	go r.runLoop(ctx, "retention", r.cfg.RetentionInterval, r.retention)
	go r.runLoop(ctx, "reconcile_guard", r.cfg.ReconcileGuardInterval, r.reconcileGuard)
	if r.cache != nil {
		go r.runLoop(ctx, "trending_sweeper", r.cfg.TrendingInterval, r.trendingSweeper)
	}
	for _, j := range r.extra {
		go r.runLoop(ctx, j.Name, j.Interval, j.Run)
	}
	r.logger.Info("search workers started")
}

func (r *Runner) runLoop(ctx context.Context, name string, interval time.Duration, job func(context.Context) error) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.runOnce(ctx, name, job)
		}
	}
}

// RunOnce executes a single job pass by name — used by integration tests to drive
// a worker deterministically without waiting for a tick.
func (r *Runner) RunOnce(ctx context.Context, name string) error {
	switch name {
	case "aggregates_rollup":
		return r.aggregatesRollup(ctx)
	case "engagement_rollup":
		return r.engagementRollup(ctx)
	case "sessionizer":
		return r.sessionizer(ctx)
	case "covis_rollup":
		return r.covisRollup(ctx)
	case "trending_sweeper":
		return r.trendingSweeper(ctx)
	case "retention":
		return r.retention(ctx)
	case "reconcile_guard":
		return r.reconcileGuard(ctx)
	}
	for _, j := range r.extra {
		if j.Name == name {
			return j.Run(ctx)
		}
	}
	return errors.New("worker: unknown job " + name)
}

func (r *Runner) runOnce(ctx context.Context, name string, job func(context.Context) error) {
	start := time.Now()
	jobCtx, cancel := context.WithTimeout(ctx, r.cfg.JobTimeout)
	defer cancel()
	if err := job(jobCtx); err != nil {
		r.logger.ErrorContext(ctx, "worker: job failed", "worker", name, "error", err)
		if r.metrics != nil {
			r.metrics.IncWorkerError(name)
		}
		return
	}
	if r.metrics != nil {
		r.metrics.ObserveRollup(name, time.Since(start).Seconds())
	}
}

// --- config overlay (service_config wins over env fallback) ---

func (r *Runner) overlay(ctx context.Context) map[string]string {
	m := map[string]string{}
	rows, err := r.store.Queries().ListServiceConfig(ctx)
	if err != nil {
		return m
	}
	for _, row := range rows {
		m[row.Key] = row.Value
	}
	return m
}

func overlayInt(m map[string]string, key string, def int) int {
	if v, ok := m[key]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// getCursor reads a worker bookmark, treating a missing row as position 0.
func getCursor(ctx context.Context, q *sqlcgen.Queries, name string) (int64, error) {
	pos, err := q.GetWorkerCursor(ctx, name)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return pos, err
}

// --- aggregates_rollup ---

func (r *Runner) aggregatesRollup(ctx context.Context) error {
	ov := r.overlay(ctx)
	minUsers := overlayInt(ov, "minimum_query_user_count", r.cfg.MinQueryUserCount)
	retentionDays := overlayInt(ov, "search_event_retention_days", r.cfg.RetentionDays)

	tx, err := r.store.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := r.store.Queries().WithTx(tx)

	cursor, err := getCursor(ctx, q, "aggregates")
	if err != nil {
		return err
	}
	maxid, err := q.MaxQueryLogID(ctx)
	if err != nil {
		return err
	}
	if maxid <= cursor {
		return tx.Commit(ctx)
	}
	windowStart := time.Now().Add(-time.Duration(retentionDays) * 24 * time.Hour)
	if err := q.RollupQueryAggregates(ctx, sqlcgen.RollupQueryAggregatesParams{
		HalfLifeSeconds: r.cfg.QueryHalfLifeSeconds,
		MinUsers:        int32(minUsers),
		FromID:          cursor,
		Maxid:           maxid,
		WindowStart:     windowStart,
	}); err != nil {
		return err
	}
	if err := q.SetWorkerCursor(ctx, sqlcgen.SetWorkerCursorParams{CursorName: "aggregates", CursorPos: maxid}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// --- engagement_rollup ---

func (r *Runner) engagementRollup(ctx context.Context) error {
	ov := r.overlay(ctx)
	mwSeconds := overlayInt(ov, "meaningful_watch_seconds", r.cfg.MeaningfulWatchSeconds)
	mwPct := overlayInt(ov, "meaningful_watch_pct", r.cfg.MeaningfulWatchPct)

	tx, err := r.store.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := r.store.Queries().WithTx(tx)

	cursor, err := getCursor(ctx, q, "engagement")
	if err != nil {
		return err
	}
	maxid, err := q.MaxBehaviorEventID(ctx)
	if err != nil {
		return err
	}
	if maxid <= cursor {
		return tx.Commit(ctx)
	}

	newMW, err := q.DeriveMeaningfulWatch(ctx, sqlcgen.DeriveMeaningfulWatchParams{
		Cursor: cursor, Maxid: maxid,
		MwSeconds: float64(mwSeconds), MwFraction: float64(mwPct) / 100.0,
	})
	if err != nil {
		return err
	}
	if err := q.FoldEngagement(ctx, sqlcgen.FoldEngagementParams{Cursor: cursor, Maxid: maxid}); err != nil {
		return err
	}

	// Apply the meaningful-watch projection weight for the rows we just derived,
	// gated by allow_history; collect the trend:v bumps to flush after commit.
	type bump struct{ item, subject string }
	var bumps []bump
	for _, mw := range newMW {
		vid, hasVideo := pgconv.UUIDValue(mw.VideoID)
		if !hasVideo {
			continue
		}
		userID, hasUser := pgconv.UUIDValue(mw.UserID)
		if hasUser && allowHistory(mw.Props) {
			if err := q.UpsertWatchProjection(ctx, sqlcgen.UpsertWatchProjectionParams{
				UserID: userID, VideoID: vid, Delta: weightMeaningfulWatch, HalfLifeHours: r.cfg.WatchHalfLifeHours,
			}); err != nil {
				return err
			}
		}
		subject := "anon"
		if hasUser {
			subject = userID.String()
		} else if mw.SessionID != nil {
			subject = *mw.SessionID
		}
		bumps = append(bumps, bump{item: vid.String(), subject: subject})
	}

	if err := q.SetWorkerCursor(ctx, sqlcgen.SetWorkerCursorParams{CursorName: "engagement", CursorPos: maxid}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}

	if r.cache != nil {
		for _, b := range bumps {
			if err := r.cache.TrendBump(ctx, "v", b.item, b.subject, r.cfg.TrendingHalfLifeSeconds, r.cfg.TrendCapWindow); err != nil {
				r.logger.WarnContext(ctx, "engagement_rollup: trend bump failed", "error", err)
			}
		}
	}
	return nil
}

func allowHistory(props []byte) bool {
	var p struct {
		AllowHistory bool `json:"allow_history"`
	}
	_ = json.Unmarshal(props, &p)
	return p.AllowHistory
}

// --- sessionizer ---

func (r *Runner) sessionizer(ctx context.Context) error {
	tx, err := r.store.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := r.store.Queries().WithTx(tx)

	cursor, err := getCursor(ctx, q, "sessionizer")
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-r.cfg.AbandonWindow)
	maxid, err := q.MaxSettledQueryLogID(ctx, cutoff)
	if err != nil {
		return err
	}
	if maxid <= cursor {
		return tx.Commit(ctx)
	}

	rows, err := q.ListQueryLogRange(ctx, sqlcgen.ListQueryLogRangeParams{Cursor: cursor, Maxid: maxid})
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		if err := q.SetWorkerCursor(ctx, sqlcgen.SetWorkerCursorParams{CursorName: "sessionizer", CursorPos: maxid}); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}

	queries := make([]QueryEvent, 0, len(rows))
	from := rows[0].SubmittedAt
	for _, row := range rows {
		if row.SubmittedAt.Before(from) {
			from = row.SubmittedAt
		}
		queries = append(queries, QueryEvent{
			ID: row.ID, NormalizedQuery: row.NormalizedQuery,
			UserID: uuidPtr(row.UserID), SessionID: derefStr(row.SessionID), SubmittedAt: row.SubmittedAt,
		})
	}
	signalRows, err := q.ListEngagementSignals(ctx, sqlcgen.ListEngagementSignalsParams{FromTs: from, ToTs: time.Now()})
	if err != nil {
		return err
	}
	signals := make([]Signal, 0, len(signalRows))
	for _, s := range signalRows {
		signals = append(signals, Signal{
			SessionID: derefStr(s.SessionID), NormalizedQuery: derefStr(s.NormalizedQuery), OccurredAt: s.OccurredAt,
		})
	}

	derived := Sessionize(queries, signals, SessionizeConfig{
		ReformulationGap: r.cfg.ReformulationGap, AbandonWindow: r.cfg.AbandonWindow,
	})
	for _, d := range derived {
		name := d.Type + "|" + d.SessionID + "|" + strconv.FormatInt(d.SourceID, 10)
		eventID := uuid.NewSHA1(derivedNamespace, []byte(name))
		props, _ := json.Marshal(map[string]string{"from": d.From, "to": d.To})
		nq := d.NormalizedQuery
		if _, err := q.InsertDerivedBehaviorEvent(ctx, sqlcgen.InsertDerivedBehaviorEventParams{
			EventID: eventID, Type: d.Type, UserID: pgconv.UUIDPtr(d.UserID),
			SessionID: strPtr(d.SessionID), NormalizedQuery: &nq, OccurredAt: d.OccurredAt, Props: props,
		}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
	}

	if err := q.SetWorkerCursor(ctx, sqlcgen.SetWorkerCursorParams{CursorName: "sessionizer", CursorPos: maxid}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// --- covis_rollup ---

// covisRollup folds new behavioral events into the cumulative co-visitation
// counters and rebuilds the served neighbor index. It is cursor-based over
// behavior_events (like the other rollups): each pass counts sessionized
// co-watch (play_started/meaningful_watch) and co-search (result_clicked sharing
// a query) pairs whose two events fall within the window, then recomputes
// shrunk-cosine neighbors (blend 0.7 co_watch / 0.3 co_search, λ shrinkage,
// top-M per item). Accumulation, rebuild, and the cursor advance share one
// transaction, so a crash resumes rather than double counts. Deterministic given
// the same events.
func (r *Runner) covisRollup(ctx context.Context) error {
	tx, err := r.store.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := r.store.Queries().WithTx(tx)

	cursor, err := getCursor(ctx, q, "covis")
	if err != nil {
		return err
	}
	maxid, err := q.MaxBehaviorEventID(ctx)
	if err != nil {
		return err
	}
	if maxid <= cursor {
		return tx.Commit(ctx)
	}

	if err := q.AccumulateCoWatch(ctx, sqlcgen.AccumulateCoWatchParams{
		Cursor: cursor, Maxid: maxid, WindowSeconds: r.cfg.CovisWindowSeconds,
	}); err != nil {
		return err
	}
	if err := q.AccumulateCoSearch(ctx, sqlcgen.AccumulateCoSearchParams{
		Cursor: cursor, Maxid: maxid, WindowSeconds: r.cfg.CovisWindowSeconds,
	}); err != nil {
		return err
	}

	// Recompute the served covis-v1 neighbor index from the current counters.
	if err := q.ClearCovisNeighbors(ctx); err != nil {
		return err
	}
	if err := q.RebuildCovisNeighbors(ctx, sqlcgen.RebuildCovisNeighborsParams{
		Lambda: r.cfg.CovisLambda, TopM: int32(r.cfg.CovisTopM),
	}); err != nil {
		return err
	}

	if err := q.SetWorkerCursor(ctx, sqlcgen.SetWorkerCursorParams{CursorName: "covis", CursorPos: maxid}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// --- trending_sweeper ---

func (r *Runner) trendingSweeper(ctx context.Context) error {
	if r.cache == nil {
		return nil
	}
	ov := r.overlay(ctx)
	minUsers := overlayInt(ov, "minimum_query_user_count", r.cfg.MinQueryUserCount)

	for _, domain := range []string{"q", "v"} {
		if _, err := r.cache.TrendSweep(ctx, domain, r.cfg.TrendingHalfLifeSeconds, r.cfg.TrendPruneFloor); err != nil {
			return err
		}
		top, err := r.cache.TrendTop(ctx, domain, r.cfg.TrendingTopK)
		if err != nil {
			return err
		}
		gated := make([]trending.Scored, 0, len(top))
		for _, cand := range top {
			distinct, err := r.cache.TrendDistinctUsers(ctx, domain, cand.Item, r.cfg.TrendWindowDays)
			if err != nil {
				return err
			}
			total, err := r.cache.TrendTotal(ctx, domain, cand.Item, r.cfg.TrendWindowDays)
			if err != nil {
				return err
			}
			ok, reason := trending.EvaluateGate(int(distinct), total, minUsers, r.cfg.WilsonFloor, trending.DefaultZ)
			if !ok {
				if r.metrics != nil {
					r.metrics.IncTrendingRejection(domain, reason)
				}
				continue
			}
			gated = append(gated, cand)
		}
		if err := r.cache.WriteTrendingList(ctx, domain, gated, r.cfg.TrendingListTTL); err != nil {
			return err
		}
	}
	return nil
}

// --- retention ---

func (r *Runner) retention(ctx context.Context) error {
	ov := r.overlay(ctx)
	retentionDays := overlayInt(ov, "search_event_retention_days", r.cfg.RetentionDays)
	q := r.store.Queries()

	// autovacuum reclaims the deleted tuples; no explicit VACUUM is needed here.
	qlN, err := q.DeleteOldQueryLog(ctx, int32(retentionDays))
	if err != nil {
		return err
	}
	beN, err := q.DeleteOldBehaviorEvents(ctx, int32(retentionDays))
	if err != nil {
		return err
	}
	inboxN, err := q.PruneEventsInbox(ctx, int32(r.cfg.InboxRetentionDays))
	if err != nil {
		return err
	}
	projN, err := q.PruneWatchProjection(ctx, sqlcgen.PruneWatchProjectionParams{
		HalfLifeHours: r.cfg.WatchHalfLifeHours, Floor: r.cfg.ProjectionFloor,
	})
	if err != nil {
		return err
	}
	r.logger.InfoContext(ctx, "retention swept",
		"query_log", qlN, "behavior_events", beN, "events_inbox", inboxN, "watch_projection", projN)
	return nil
}

// --- reconcile_guard ---

func (r *Runner) reconcileGuard(ctx context.Context) error {
	age, err := r.store.Queries().LastReconcileEndAgeSeconds(ctx)
	if err != nil {
		return err
	}
	if r.metrics != nil {
		r.metrics.SetReconcileAge(age)
	}
	if age < 0 || age > r.cfg.ReconcileMaxAge.Seconds() {
		r.logger.WarnContext(ctx, "reconcile guard: index may be stale — no recent reconcile.end",
			"age_seconds", age, "max_age_seconds", r.cfg.ReconcileMaxAge.Seconds())
	}
	return nil
}

// --- small helpers ---

func uuidPtr(v pgtype.UUID) *uuid.UUID {
	if id, ok := pgconv.UUIDValue(v); ok {
		return &id
	}
	return nil
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func strPtr(s string) *string { return &s }
