// Package api wires the Echo HTTP server for vidra-search: routing, middleware,
// and the thin request/response handlers. Application logic lives in the service
// packages (suggest, search, recommendation, event). All business endpoints sit
// under /internal/v1 behind HMAC authentication; only the ops probes
// (/healthz, /readyz, /version, /metrics) are public and excluded from the
// OpenAPI contract.
package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"

	"github.com/vidra/vidra-search/internal/config"
	"github.com/vidra/vidra-search/internal/event"
	"github.com/vidra/vidra-search/internal/recommendation"
	"github.com/vidra/vidra-search/internal/search"
	"github.com/vidra/vidra-search/internal/suggest"
	"github.com/vidra/vidra-search/internal/telemetry"
)

// Pinger is satisfied by dependencies that can report liveness (store, cache).
type Pinger interface {
	Ping(ctx context.Context) error
}

// Services bundles the domain services the handlers delegate to. Any may be nil
// for the route-contract test, which only inspects the routing table.
type Services struct {
	Suggest *suggest.Service
	Search  *search.Service
	Rec     *recommendation.Service
	Events  *event.Service
}

// Server holds the Echo instance and its dependencies.
type Server struct {
	echo      *echo.Echo
	cfg       *config.Config
	logger    *slog.Logger
	metrics   *telemetry.Metrics
	db        Pinger
	rdb       Pinger
	svcs      Services
	startedAt time.Time
}

// New constructs the server: middleware stack, routes, and the central error
// handler. metrics may be nil (METRICS_ENABLED=false); db/rdb may be nil (the
// readiness probe then reports them "not_configured").
func New(cfg *config.Config, logger *slog.Logger, metrics *telemetry.Metrics, db, rdb Pinger, svcs Services) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	e := echo.New()
	e.HideBanner = true
	e.HidePort = true

	s := &Server{
		echo:      e,
		cfg:       cfg,
		logger:    logger,
		metrics:   metrics,
		db:        db,
		rdb:       rdb,
		svcs:      svcs,
		startedAt: time.Now(),
	}
	e.HTTPErrorHandler = s.httpErrorHandler

	e.Use(middleware.Recover())
	e.Use(secureHeaders())
	e.Use(middleware.RequestID())
	e.Use(correlationID())
	e.Use(s.requestLogger())
	e.Use(requestDeadline(cfg.HTTPRequestTimeout))
	e.Use(middleware.BodyLimitWithConfig(middleware.BodyLimitConfig{Limit: cfg.HTTPBodyLimit}))

	s.routes()
	return s
}

// Handler exposes the underlying Echo instance (used by tests and the contract
// route enumerator).
func (s *Server) Handler() *echo.Echo { return s.echo }

// Start runs the HTTP server until ctx is cancelled, then gracefully shuts down.
func (s *Server) Start(ctx context.Context) error {
	s.echo.Server.ReadTimeout = s.cfg.HTTPReadTimeout
	s.echo.Server.WriteTimeout = s.cfg.HTTPWriteTimeout

	errCh := make(chan error, 1)
	go func() {
		if err := s.echo.Start(s.cfg.HTTPAddr()); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), s.cfg.HTTPShutdownTimeout)
		defer cancel()
		return s.echo.Shutdown(shutdownCtx)
	}
}

func (s *Server) routes() {
	s.echo.GET("/healthz", s.handleLive)
	s.echo.GET("/readyz", s.handleReady)
	s.echo.GET("/version", s.handleVersion)
	if s.metrics != nil {
		s.echo.GET("/metrics", echo.WrapHandler(s.metrics.Handler()))
	}

	// All business endpoints live under /internal/v1 behind HMAC auth. They are
	// registered unconditionally so the OpenAPI drift guard always sees the full
	// surface (the handlers no-op safely when a service is nil in tests).
	g := s.echo.Group("/internal/v1", internalAuth(s.cfg.InternalSecret))
	g.GET("/suggestions", s.handleSuggestions)
	g.GET("/search", s.handleSearch)
	g.GET("/recommendations/related", s.handleRelated)
	g.GET("/recommendations/home", s.handleHome)
	g.POST("/events", s.handleEvents)
}

// requestLogger emits one structured slog line per request and, when metrics are
// enabled, records the RED metrics from the same choke point. Only the bounded
// route template is logged — never the raw URL/query.
func (s *Server) requestLogger() echo.MiddlewareFunc {
	return middleware.RequestLoggerWithConfig(middleware.RequestLoggerConfig{
		LogStatus:    true,
		LogMethod:    true,
		LogLatency:   true,
		LogRequestID: true,
		LogError:     true,
		HandleError:  true,
		LogValuesFunc: func(c echo.Context, v middleware.RequestLoggerValues) error {
			level := slog.LevelInfo
			switch {
			case v.Status >= 500:
				level = slog.LevelError
			case v.Status >= 400:
				level = slog.LevelWarn
			}
			attrs := []any{
				"method", v.Method,
				"path", requestLogPath(c),
				"status", v.Status,
				"latency_ms", v.Latency.Milliseconds(),
				"request_id", v.RequestID,
			}
			if cid := correlationFromContext(c); cid != "" {
				attrs = append(attrs, "correlation_id", cid)
			}
			if v.Error != nil {
				attrs = append(attrs, "error", v.Error)
			}
			s.logger.Log(c.Request().Context(), level, "request", attrs...)
			if s.metrics != nil {
				s.metrics.ObserveRequest(v.Method, c.Path(), v.Status, v.Latency)
			}
			return nil
		},
	})
}

func requestLogPath(c echo.Context) string {
	if path := c.Path(); path != "" {
		return path
	}
	return c.Request().URL.Path
}

// requestDeadline attaches a timeout to each request's context so handlers and
// their DB/Redis calls observe a deadline and abort cleanly.
func requestDeadline(d time.Duration) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			ctx, cancel := context.WithTimeout(c.Request().Context(), d)
			defer cancel()
			c.SetRequest(c.Request().WithContext(ctx))
			return next(c)
		}
	}
}

// secureHeaders adds conservative response headers. The service is internal, so
// this is defense in depth rather than a browser-facing hardening surface.
func secureHeaders() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			c.Response().Header().Set("X-Content-Type-Options", "nosniff")
			return next(c)
		}
	}
}
