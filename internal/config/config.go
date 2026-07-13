// Package config loads and validates vidra-search configuration from the
// environment. Configuration is the single source of truth for runtime wiring;
// no other package reads os.Getenv directly. It mirrors vidra-core's flat
// Config + Load() + validate() convention.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/gommon/bytes"
)

// devInternalSecret is the obviously-fake HMAC secret used only for local
// dev/test. Production must override INTERNAL_SECRET; validate() rejects this
// value (and any secret shorter than 32 bytes) in production.
const devInternalSecret = "dev-insecure-internal-secret-change-me-0000"

// Config holds all runtime configuration for the vidra-search service.
type Config struct {
	// Environment is one of "development", "test", or "production".
	Environment string

	// Logging. LogLevel is the minimum emitted level (debug|info|warn|error);
	// LogFormat selects the slog handler (json — default — or text).
	LogLevel  string
	LogFormat string

	// MetricsEnabled gates the Prometheus scrape surface (/metrics + the
	// request-metrics middleware). Opt-in and zero-cost when false.
	MetricsEnabled bool

	// HTTP server.
	HTTPHost            string
	HTTPPort            int
	HTTPReadTimeout     time.Duration
	HTTPWriteTimeout    time.Duration
	HTTPShutdownTimeout time.Duration
	// HTTPRequestTimeout bounds per-request handler work via a context deadline.
	HTTPRequestTimeout time.Duration
	// HTTPBodyLimit is the maximum accepted request body (Echo size string).
	HTTPBodyLimit string

	// PostgreSQL connection (DSN form).
	DatabaseURL string
	// Redis connection (URL form).
	RedisURL string

	// InternalSecret is the shared HMAC secret authenticating vidra-core's calls
	// to /internal/v1/* (X-Vidra-Internal-Auth). Required (>=32 bytes) in
	// production; a dev default is used otherwise.
	InternalSecret string

	// Policy knobs. These mirror the service_config values vidra-core pushes via
	// search.config_updated; the env value is the boot-time fallback.
	MinQueryUserCount      int
	EventRetentionDays     int
	TrendingHalfLifeHours  float64
	MeaningfulWatchSeconds int
	MeaningfulWatchPct     int

	// Suggestion recency decay half-life (query_aggregates.decayed_freq). Slower
	// than trending so popular queries stay suggestible for days.
	QueryHalfLifeHours float64
	// Watch-affinity decay half-life (user_watch_projection).
	WatchHalfLifeHours float64
	// TrendCapWindow bounds how often one (subject,item) may bump a trend score.
	TrendCapWindow time.Duration
	// TrendingWilsonFloor is the Wilson lower-bound min-volume gate for trending.
	TrendingWilsonFloor float64

	// WorkersEnabled gates the background rollup loops.
	WorkersEnabled bool
	// Worker cadences (§1.9).
	AggregatesInterval     time.Duration
	EngagementInterval     time.Duration
	SessionizerInterval    time.Duration
	TrendingInterval       time.Duration
	CovisInterval          time.Duration
	RetentionInterval      time.Duration
	ReconcileGuardInterval time.Duration

	// Co-visitation tuning (§1.9). Window bounds the in-session gap for a pair;
	// lambda is the cosine shrinkage; top-M is neighbors kept per item.
	CovisWindowSeconds float64
	CovisLambda        float64
	CovisTopM          int

	// ModelDir is where the model registry keeps artifacts (ranker LightGBM text
	// files). The model_loader + shadow-eval workers read/verify artifacts here.
	ModelDir string

	// ModelLoaderInterval / ShadowEvalInterval are the model worker cadences.
	ModelLoaderInterval time.Duration
	ShadowEvalInterval  time.Duration
	// ShadowEvalDays is the look-back window of logged impressions replayed.
	ShadowEvalDays int
}

// Load reads configuration from the environment, applying safe development
// defaults, and validates it. Required in production: DATABASE_URL, REDIS_URL,
// a strong INTERNAL_SECRET; in development they default to the standalone
// docker-compose service addresses.
func Load() (*Config, error) {
	cfg := &Config{
		Environment:         getEnv("VIDRA_ENV", "development"),
		LogLevel:            strings.ToLower(getEnv("LOG_LEVEL", "info")),
		LogFormat:           strings.ToLower(getEnv("LOG_FORMAT", "json")),
		MetricsEnabled:      getEnvBool("METRICS_ENABLED", false),
		HTTPHost:            getEnv("HTTP_HOST", "0.0.0.0"),
		HTTPReadTimeout:     getEnvDuration("HTTP_READ_TIMEOUT", 15*time.Second),
		HTTPWriteTimeout:    getEnvDuration("HTTP_WRITE_TIMEOUT", 30*time.Second),
		HTTPShutdownTimeout: getEnvDuration("HTTP_SHUTDOWN_TIMEOUT", 20*time.Second),
		HTTPRequestTimeout:  getEnvDuration("HTTP_REQUEST_TIMEOUT", 10*time.Second),
		HTTPBodyLimit:       getEnv("HTTP_BODY_LIMIT", "2M"),
		DatabaseURL:         getEnv("DATABASE_URL", "postgres://vidra_search:vidra_search@localhost:5433/vidra_search?sslmode=disable"),
		RedisURL:            getEnv("REDIS_URL", "redis://localhost:6380/0"),
		InternalSecret:      getEnv("INTERNAL_SECRET", devInternalSecret),

		TrendCapWindow:         getEnvDuration("SEARCH_TREND_CAP_WINDOW", time.Hour),
		WorkersEnabled:         getEnvBool("SEARCH_WORKERS_ENABLED", true),
		AggregatesInterval:     getEnvDuration("SEARCH_AGGREGATES_INTERVAL", time.Minute),
		EngagementInterval:     getEnvDuration("SEARCH_ENGAGEMENT_INTERVAL", 5*time.Minute),
		SessionizerInterval:    getEnvDuration("SEARCH_SESSIONIZER_INTERVAL", 5*time.Minute),
		TrendingInterval:       getEnvDuration("SEARCH_TRENDING_INTERVAL", time.Minute),
		CovisInterval:          getEnvDuration("SEARCH_COVIS_INTERVAL", 15*time.Minute),
		RetentionInterval:      getEnvDuration("SEARCH_RETENTION_INTERVAL", 24*time.Hour),
		ReconcileGuardInterval: getEnvDuration("SEARCH_RECONCILE_GUARD_INTERVAL", 10*time.Minute),
		ModelDir:               getEnv("MODEL_DIR", "/var/lib/vidra-search/models"),
		ModelLoaderInterval:    getEnvDuration("SEARCH_MODEL_LOADER_INTERVAL", time.Minute),
		ShadowEvalInterval:     getEnvDuration("SEARCH_SHADOW_EVAL_INTERVAL", time.Hour),
	}

	port, err := getEnvInt("HTTP_PORT", 8080)
	if err != nil {
		return nil, err
	}
	cfg.HTTPPort = port

	if cfg.MinQueryUserCount, err = getEnvInt("MIN_QUERY_USER_COUNT", 3); err != nil {
		return nil, err
	}
	if cfg.EventRetentionDays, err = getEnvInt("EVENT_RETENTION_DAYS", 90); err != nil {
		return nil, err
	}
	if cfg.TrendingHalfLifeHours, err = getEnvFloat("TRENDING_HALF_LIFE_HOURS", 6); err != nil {
		return nil, err
	}
	if cfg.MeaningfulWatchSeconds, err = getEnvInt("MEANINGFUL_WATCH_SECONDS", 30); err != nil {
		return nil, err
	}
	if cfg.MeaningfulWatchPct, err = getEnvInt("MEANINGFUL_WATCH_PCT", 30); err != nil {
		return nil, err
	}
	if cfg.QueryHalfLifeHours, err = getEnvFloat("SEARCH_QUERY_HALF_LIFE_HOURS", 168); err != nil {
		return nil, err
	}
	if cfg.WatchHalfLifeHours, err = getEnvFloat("SEARCH_WATCH_HALF_LIFE_HOURS", 720); err != nil {
		return nil, err
	}
	if cfg.TrendingWilsonFloor, err = getEnvFloat("SEARCH_TRENDING_WILSON_FLOOR", 0.10); err != nil {
		return nil, err
	}
	if cfg.CovisWindowSeconds, err = getEnvFloat("SEARCH_COVIS_WINDOW_SECONDS", 3600); err != nil {
		return nil, err
	}
	if cfg.CovisLambda, err = getEnvFloat("SEARCH_COVIS_LAMBDA", 10); err != nil {
		return nil, err
	}
	if cfg.CovisTopM, err = getEnvInt("SEARCH_COVIS_TOP_M", 100); err != nil {
		return nil, err
	}
	if cfg.ShadowEvalDays, err = getEnvInt("SEARCH_SHADOW_EVAL_DAYS", 14); err != nil {
		return nil, err
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) validate() error {
	switch c.Environment {
	case "development", "test", "production":
	default:
		return fmt.Errorf("config: invalid VIDRA_ENV %q (want development|test|production)", c.Environment)
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("config: invalid LOG_LEVEL %q (want debug|info|warn|error)", c.LogLevel)
	}
	switch c.LogFormat {
	case "json", "text":
	default:
		return fmt.Errorf("config: invalid LOG_FORMAT %q (want json|text)", c.LogFormat)
	}
	if c.HTTPPort < 1 || c.HTTPPort > 65535 {
		return fmt.Errorf("config: HTTP_PORT %d out of range", c.HTTPPort)
	}
	if strings.TrimSpace(c.DatabaseURL) == "" {
		return fmt.Errorf("config: DATABASE_URL is required")
	}
	if strings.TrimSpace(c.RedisURL) == "" {
		return fmt.Errorf("config: REDIS_URL is required")
	}
	if c.HTTPRequestTimeout <= 0 {
		return fmt.Errorf("config: HTTP_REQUEST_TIMEOUT must be positive")
	}
	if _, err := bytes.Parse(c.HTTPBodyLimit); err != nil {
		return fmt.Errorf("config: invalid HTTP_BODY_LIMIT %q: %w", c.HTTPBodyLimit, err)
	}
	if c.MinQueryUserCount < 1 {
		return fmt.Errorf("config: MIN_QUERY_USER_COUNT must be >= 1")
	}
	if c.EventRetentionDays < 1 {
		return fmt.Errorf("config: EVENT_RETENTION_DAYS must be >= 1")
	}
	if c.TrendingHalfLifeHours <= 0 {
		return fmt.Errorf("config: TRENDING_HALF_LIFE_HOURS must be positive")
	}
	if c.MeaningfulWatchSeconds < 1 {
		return fmt.Errorf("config: MEANINGFUL_WATCH_SECONDS must be >= 1")
	}
	if c.MeaningfulWatchPct < 1 || c.MeaningfulWatchPct > 100 {
		return fmt.Errorf("config: MEANINGFUL_WATCH_PCT must be in [1,100]")
	}
	if c.QueryHalfLifeHours <= 0 {
		return fmt.Errorf("config: SEARCH_QUERY_HALF_LIFE_HOURS must be positive")
	}
	if c.WatchHalfLifeHours <= 0 {
		return fmt.Errorf("config: SEARCH_WATCH_HALF_LIFE_HOURS must be positive")
	}
	if c.TrendingWilsonFloor < 0 || c.TrendingWilsonFloor > 1 {
		return fmt.Errorf("config: SEARCH_TRENDING_WILSON_FLOOR must be in [0,1]")
	}
	if c.TrendCapWindow <= 0 {
		return fmt.Errorf("config: SEARCH_TREND_CAP_WINDOW must be positive")
	}
	if c.CovisWindowSeconds <= 0 {
		return fmt.Errorf("config: SEARCH_COVIS_WINDOW_SECONDS must be positive")
	}
	if c.CovisLambda < 0 {
		return fmt.Errorf("config: SEARCH_COVIS_LAMBDA must be >= 0")
	}
	if c.CovisTopM < 1 {
		return fmt.Errorf("config: SEARCH_COVIS_TOP_M must be >= 1")
	}
	if c.ShadowEvalDays < 1 {
		return fmt.Errorf("config: SEARCH_SHADOW_EVAL_DAYS must be >= 1")
	}
	if c.Environment == "production" {
		if c.InternalSecret == devInternalSecret {
			return fmt.Errorf("config: INTERNAL_SECRET must be set in production (the dev default is not allowed)")
		}
		if len(c.InternalSecret) < 32 {
			return fmt.Errorf("config: INTERNAL_SECRET must be at least 32 bytes in production")
		}
	}
	return nil
}

// HTTPAddr returns the host:port the HTTP server should bind to.
func (c *Config) HTTPAddr() string {
	return fmt.Sprintf("%s:%d", c.HTTPHost, c.HTTPPort)
}

// UsingDevInternalSecret reports whether the insecure development HMAC secret is
// in effect, so cmd/api can warn loudly at boot outside production.
func (c *Config) UsingDevInternalSecret() bool {
	return c.InternalSecret == devInternalSecret
}

func getEnv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func getEnvInt(key string, def int) (int, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("config: %s must be an integer: %w", key, err)
	}
	return n, nil
}

func getEnvFloat(key string, def float64) (float64, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, fmt.Errorf("config: %s must be a number: %w", key, err)
	}
	return f, nil
}

func getEnvBool(key string, def bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func getEnvDuration(key string, def time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}
