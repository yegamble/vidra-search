package config

import (
	"strings"
	"testing"
)

// setEnv sets envs for one test and restores them via t.Cleanup.
func setEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

func TestLoadDefaults(t *testing.T) {
	// t.Setenv guarantees a clean, isolated environment per test.
	setEnv(t, map[string]string{"VIDRA_ENV": "development"})
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTPPort != 8080 {
		t.Errorf("HTTPPort = %d, want 8080", cfg.HTTPPort)
	}
	if cfg.MinQueryUserCount != 3 {
		t.Errorf("MinQueryUserCount = %d, want 3", cfg.MinQueryUserCount)
	}
	if cfg.EventRetentionDays != 90 {
		t.Errorf("EventRetentionDays = %d, want 90", cfg.EventRetentionDays)
	}
	if cfg.TrendingHalfLifeHours != 6 {
		t.Errorf("TrendingHalfLifeHours = %v, want 6", cfg.TrendingHalfLifeHours)
	}
	if cfg.MeaningfulWatchSeconds != 30 || cfg.MeaningfulWatchPct != 30 {
		t.Errorf("meaningful watch = %d/%d, want 30/30", cfg.MeaningfulWatchSeconds, cfg.MeaningfulWatchPct)
	}
	if !cfg.UsingDevInternalSecret() {
		t.Errorf("expected dev internal secret in development")
	}
	if cfg.HTTPRequestTimeout.Seconds() != 10 {
		t.Errorf("HTTPRequestTimeout = %v, want 10s", cfg.HTTPRequestTimeout)
	}
}

func TestLoadProductionRequiresStrongSecret(t *testing.T) {
	setEnv(t, map[string]string{
		"VIDRA_ENV":    "production",
		"DATABASE_URL": "postgres://x/y",
		"REDIS_URL":    "redis://localhost:6379/0",
	})
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "INTERNAL_SECRET") {
		t.Fatalf("expected INTERNAL_SECRET error in production, got %v", err)
	}

	// Too-short secret still fails.
	t.Setenv("INTERNAL_SECRET", "short")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "32 bytes") {
		t.Fatalf("expected length error, got %v", err)
	}

	// A strong secret passes.
	t.Setenv("INTERNAL_SECRET", strings.Repeat("a", 40))
	if _, err := Load(); err != nil {
		t.Fatalf("expected success with strong secret, got %v", err)
	}
}

func TestLoadRejectsBadValues(t *testing.T) {
	cases := map[string]map[string]string{
		"bad env":        {"VIDRA_ENV": "staging"},
		"bad log level":  {"VIDRA_ENV": "development", "LOG_LEVEL": "loud"},
		"bad log format": {"VIDRA_ENV": "development", "LOG_FORMAT": "xml"},
		"bad port":       {"VIDRA_ENV": "development", "HTTP_PORT": "70000"},
		"bad watch pct":  {"VIDRA_ENV": "development", "MEANINGFUL_WATCH_PCT": "200"},
		"bad body limit": {"VIDRA_ENV": "development", "HTTP_BODY_LIMIT": "banana"},
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			setEnv(t, env)
			if _, err := Load(); err == nil {
				t.Fatalf("expected error for %s", name)
			}
		})
	}
}
