package model

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"sync/atomic"

	"github.com/jackc/pgx/v5"

	"github.com/vidra/vidra-search/internal/ranking"
	"github.com/vidra/vidra-search/internal/store/sqlcgen"
)

// Querier is the store surface the loader reads the model registry through.
type Querier interface {
	GetActiveModel(ctx context.Context, kind string) (sqlcgen.SearchModel, error)
}

// Metrics is the loader's telemetry seam (nil-safe).
type Metrics interface {
	IncModelLoadError()
	SetLoadedModel(kind, version string)
}

// loaded is the currently-served learned ranker plus the registry identity it was
// built from, so Refresh can detect an unchanged active model and skip reloading.
type loaded struct {
	ranker  *Learned
	version string
	sha     string
}

// Loader owns the active learned ranker behind an atomic pointer so serving reads
// are lock-free and a hot-swap is a single pointer store. When the pointer is nil
// there is no learned model and callers fall back to the heuristic.
type Loader struct {
	dir       string
	q         Querier
	logger    *slog.Logger
	metrics   Metrics
	penalty   float64
	current   atomic.Pointer[loaded]
	heuristic *Heuristic
}

// NewLoader builds a loader rooted at modelDir. penalty is the creator-repetition
// demotion applied on top of a learned model's scores. metrics may be nil.
func NewLoader(modelDir string, q Querier, penalty float64, metrics Metrics, logger *slog.Logger) *Loader {
	if logger == nil {
		logger = slog.Default()
	}
	return &Loader{
		dir: modelDir, q: q, logger: logger, metrics: metrics,
		penalty: penalty, heuristic: NewHeuristic(),
	}
}

// Heuristic returns the always-available heuristic ranker.
func (l *Loader) Heuristic() *Heuristic { return l.heuristic }

// Learned returns the currently-loaded learned ranker and its version, or (nil,
// "") when none is loaded. Lock-free.
func (l *Loader) Learned() (*Learned, string) {
	if cur := l.current.Load(); cur != nil {
		return cur.ranker, cur.version
	}
	return nil, ""
}

// RankerFor returns the ranker to serve given the model version an experiment
// routes to (wantVersion). When wantVersion names the currently-loaded learned
// model it is served; otherwise the heuristic is returned. An empty wantVersion
// always yields the heuristic. The returned string is the version to report/stamp.
func (l *Loader) RankerFor(wantVersion string) (ranking.Ranker, string) {
	if wantVersion != "" {
		if learned, v := l.Learned(); learned != nil && v == wantVersion {
			return learned, v
		}
	}
	return l.heuristic, l.heuristic.Version()
}

// Refresh reconciles the served model with the registry: it loads the active
// ranker artifact if it changed, and clears the learned model when no active
// ranker exists. A missing / bad-checksum / malformed artifact is logged, counted
// (metric), and leaves BOTH the current in-memory model AND the models row
// unchanged — the service keeps serving the previous model (or the heuristic).
func (l *Loader) Refresh(ctx context.Context) error {
	m, err := l.q.GetActiveModel(ctx, "ranker")
	if errors.Is(err, pgx.ErrNoRows) {
		// No active ranker → ensure we are on the heuristic.
		if l.current.Swap(nil) != nil {
			l.logger.InfoContext(ctx, "model: no active ranker; reverted to heuristic")
			if l.metrics != nil {
				l.metrics.SetLoadedModel("ranker", HeuristicVersion)
			}
		}
		return nil
	}
	if err != nil {
		return err
	}

	sha := deref(m.ArtifactSha256)
	if cur := l.current.Load(); cur != nil && cur.version == m.Version && cur.sha == sha {
		return nil // already serving this exact model
	}

	path := l.artifactPath(m)
	lm, err := LoadLeaves(path, sha)
	if err != nil {
		// Keep serving whatever we have; do NOT touch the models row.
		l.logger.ErrorContext(ctx, "model: active artifact failed to load; keeping previous ranker",
			"version", m.Version, "path", path, "error", err)
		if l.metrics != nil {
			l.metrics.IncModelLoadError()
		}
		return nil
	}

	l.current.Store(&loaded{
		ranker:  NewLearned(lm, m.Version, l.penalty),
		version: m.Version,
		sha:     sha,
	})
	l.logger.InfoContext(ctx, "model: loaded active ranker", "version", m.Version, "path", path, "features", lm.NFeatures())
	if l.metrics != nil {
		l.metrics.SetLoadedModel("ranker", m.Version)
	}
	return nil
}

// LoadModel loads a registry row's artifact as a Learned ranker (verifying its
// SHA-256), using the loader's model dir + creator penalty. Used by shadow
// evaluation to score a shadow model that is NOT the served (active) one.
func (l *Loader) LoadModel(m sqlcgen.SearchModel) (*Learned, error) {
	lm, err := LoadLeaves(l.artifactPath(m), deref(m.ArtifactSha256))
	if err != nil {
		return nil, err
	}
	return NewLearned(lm, m.Version, l.penalty), nil
}

// artifactPath resolves the artifact location: the stored absolute/relative
// artifact_path when present, else MODEL_DIR/ranker-{version}.txt.
func (l *Loader) artifactPath(m sqlcgen.SearchModel) string {
	if p := deref(m.ArtifactPath); p != "" {
		if filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(l.dir, p)
	}
	return filepath.Join(l.dir, "ranker-"+m.Version+".txt")
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// interface assertions.
var (
	_ Ranker = (*Heuristic)(nil)
	_ Ranker = (*Learned)(nil)
	_        = ranking.DefaultAdvancedWeights
)
