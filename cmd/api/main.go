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

	"github.com/vidra/vidra-search/internal/api"
	"github.com/vidra/vidra-search/internal/cache"
	"github.com/vidra/vidra-search/internal/config"
	"github.com/vidra/vidra-search/internal/event"
	"github.com/vidra/vidra-search/internal/recommendation"
	"github.com/vidra/vidra-search/internal/search"
	"github.com/vidra/vidra-search/internal/store"
	"github.com/vidra/vidra-search/internal/suggest"
	"github.com/vidra/vidra-search/internal/telemetry"
	"github.com/vidra/vidra-search/internal/version"
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
	}

	q := st.Queries()
	svcs := api.Services{
		Suggest: suggest.NewService(q, suggest.NoopAggregate{}, rdb, logger),
		Search:  search.NewService(q),
		Rec:     recommendation.NewService(q),
		Events:  event.NewService(st, metrics, logger),
	}

	srv := api.New(cfg, logger, metrics, st, rdb, svcs)
	logger.Info("listening", "addr", cfg.HTTPAddr())
	return srv.Start(ctx)
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
