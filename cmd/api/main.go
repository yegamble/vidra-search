// Command api is the vidra-search service: an HTTP server exposing the internal
// search/suggestion/recommendation/event API under /internal/v1 (HMAC-
// authenticated) plus the ops probes. Configuration comes entirely from the
// environment (internal/config).
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/vidra/vidra-search/internal/api"
	"github.com/vidra/vidra-search/internal/cache"
	"github.com/vidra/vidra-search/internal/config"
	"github.com/vidra/vidra-search/internal/event"
	"github.com/vidra/vidra-search/internal/experiment"
	"github.com/vidra/vidra-search/internal/history"
	"github.com/vidra/vidra-search/internal/model"
	"github.com/vidra/vidra-search/internal/ranking"
	"github.com/vidra/vidra-search/internal/recommendation"
	"github.com/vidra/vidra-search/internal/search"
	"github.com/vidra/vidra-search/internal/store"
	"github.com/vidra/vidra-search/internal/suggest"
	"github.com/vidra/vidra-search/internal/telemetry"
	"github.com/vidra/vidra-search/internal/version"
	"github.com/vidra/vidra-search/internal/worker"
)

func main() {
	if err := run(); err != nil {
		slog.Error("vidra-search: fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger, err := telemetry.NewLogger(os.Stdout, cfg.LogLevel, cfg.LogFormat)
	if err != nil {
		return err
	}
	slog.SetDefault(logger)
	logger.Info("starting vidra-search",
		"version", version.Version, "commit", version.Commit, "env", cfg.Environment)
	if cfg.UsingDevInternalSecret() && cfg.Environment != "production" {
		logger.Warn("using the INSECURE development INTERNAL_SECRET — set INTERNAL_SECRET for any shared deployment")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := store.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()

	rdb, err := cache.New(ctx, cfg.RedisURL)
	if err != nil {
		return err
	}
	defer func() { _ = rdb.Close() }()

	var metrics *telemetry.Metrics
	if cfg.MetricsEnabled {
		metrics = telemetry.NewMetrics()
		metrics.RegisterDocumentGaugeSource(documentGaugeSource(st))
		metrics.RegisterTableDepthSource(tableDepthSource(st))
	}

	// event.Metrics / worker.Metrics are satisfied by *telemetry.Metrics only when
	// metrics are enabled; use true-nil interfaces (not a typed nil pointer)
	// otherwise so the services' nil guards engage.
	var em event.Metrics
	var wm worker.Metrics
	var mm model.Metrics
	var sm model.ShadowMetrics
	if metrics != nil {
		em = metrics
		wm = metrics
		mm = metrics
		sm = metrics
	}

	q := st.Queries()
	eventCfg := event.Config{
		TrendingHalfLifeSeconds: cfg.TrendingHalfLifeHours * 3600,
		TrendCapWindow:          cfg.TrendCapWindow,
		WatchHalfLifeHours:      cfg.WatchHalfLifeHours,
	}

	// Model registry + serving (§1.9): load the active learned ranker if present,
	// else serve the always-available heuristic.
	loader := model.NewLoader(cfg.ModelDir, q, ranking.DefaultAdvancedWeights.CreatorPenalty, mm, logger)
	if err := loader.Refresh(ctx); err != nil {
		logger.Warn("model: initial load failed; serving heuristic", "error", err)
	}
	// Experiment definitions cached in RAM (§1.5).
	experiments := experiment.NewRegistry(q, logger)
	if err := experiments.Refresh(ctx); err != nil {
		logger.Warn("experiment: initial refresh failed", "error", err)
	}
	evaluator := model.NewShadowEvaluator(q, loader, sm, logger, cfg.ShadowEvalDays)

	svcs := api.Services{
		Suggest: suggest.NewService(q, suggest.NewStoreAggregate(q), rdb, rdb, rdb, logger),
		Search:  search.NewService(q, loader, experiments, rdb, logger),
		Rec:     recommendation.NewService(q, rdb, loader, experiments, rdb, logger),
		Events:  event.NewService(st, em, logger, eventCfg, rdb),
		History: history.NewService(st),
	}

	runner := worker.NewRunner(st, rdb, workerConfig(cfg), wm, logger)
	runner.AddJob(worker.PeriodicJob{Name: "model_loader", Interval: cfg.ModelLoaderInterval, Run: loader.Refresh})
	runner.AddJob(worker.PeriodicJob{Name: "experiment_refresh", Interval: 5 * time.Minute, Run: experiments.Refresh})
	runner.AddJob(worker.PeriodicJob{Name: "shadow_eval", Interval: cfg.ShadowEvalInterval, Run: evaluator.Run})

	// One-shot job mode (e.g. `make shadow-eval`): run the named job once and exit.
	if cfg.RunJob != "" {
		logger.Info("running one-shot job", "job", cfg.RunJob)
		if err := runner.RunOnce(ctx, cfg.RunJob); err != nil {
			return err
		}
		return nil
	}

	if cfg.WorkersEnabled {
		runner.Start(ctx)
	}

	srv := api.New(cfg, logger, metrics, st, rdb, svcs)
	logger.Info("listening", "addr", cfg.HTTPAddr())
	return srv.Start(ctx)
}

// workerConfig maps the flat service config onto the worker tuning struct.
func workerConfig(cfg *config.Config) worker.Config {
	return worker.Config{
		AggregatesInterval:      cfg.AggregatesInterval,
		EngagementInterval:      cfg.EngagementInterval,
		SessionizerInterval:     cfg.SessionizerInterval,
		TrendingInterval:        cfg.TrendingInterval,
		CovisInterval:           cfg.CovisInterval,
		RetentionInterval:       cfg.RetentionInterval,
		ReconcileGuardInterval:  cfg.ReconcileGuardInterval,
		MinQueryUserCount:       cfg.MinQueryUserCount,
		RetentionDays:           cfg.EventRetentionDays,
		QueryHalfLifeSeconds:    cfg.QueryHalfLifeHours * 3600,
		TrendingHalfLifeSeconds: cfg.TrendingHalfLifeHours * 3600,
		WatchHalfLifeHours:      cfg.WatchHalfLifeHours,
		TrendCapWindow:          cfg.TrendCapWindow,
		MeaningfulWatchSeconds:  cfg.MeaningfulWatchSeconds,
		MeaningfulWatchPct:      cfg.MeaningfulWatchPct,
		WilsonFloor:             cfg.TrendingWilsonFloor,
		CovisWindowSeconds:      cfg.CovisWindowSeconds,
		CovisLambda:             cfg.CovisLambda,
		CovisTopM:               cfg.CovisTopM,
	}
}

// tableDepthSource samples approximate row counts for the search schema's tables
// from the planner statistics (cheap; run at scrape time). It uses the raw pool
// because pg_class is a system catalog outside sqlc's analyzed schema.
func tableDepthSource(st *store.Store) func(context.Context) ([]telemetry.TableDepth, error) {
	const query = `SELECT c.relname, GREATEST(c.reltuples, 0)::bigint
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = 'search' AND c.relkind = 'r'`
	return func(ctx context.Context) ([]telemetry.TableDepth, error) {
		rows, err := st.Pool.Query(ctx, query)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []telemetry.TableDepth
		for rows.Next() {
			var d telemetry.TableDepth
			if err := rows.Scan(&d.Table, &d.Rows); err != nil {
				return nil, err
			}
			out = append(out, d)
		}
		return out, rows.Err()
	}
}

// documentGaugeSource adapts the store's eligibility count query to the metrics
// gauge source shape.
func documentGaugeSource(st *store.Store) func(context.Context) ([]telemetry.DocCount, error) {
	return func(ctx context.Context) ([]telemetry.DocCount, error) {
		rows, err := st.Queries().CountDocumentsByEligibility(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]telemetry.DocCount, 0, len(rows))
		for _, r := range rows {
			out = append(out, telemetry.DocCount{Eligible: r.Eligible, Count: r.Count})
		}
		return out, nil
	}
}
