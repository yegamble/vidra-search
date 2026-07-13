package model

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/vidra/vidra-search/internal/ranking"
	"github.com/vidra/vidra-search/internal/store/sqlcgen"
)

// fakeLoaderQ is a fake model registry for the loader.
type fakeLoaderQ struct {
	active sqlcgen.SearchModel
	err    error
}

func (f fakeLoaderQ) GetActiveModel(context.Context, string) (sqlcgen.SearchModel, error) {
	return f.active, f.err
}

// countMetrics records loader telemetry for assertions.
type countMetrics struct {
	loadErrors int
	loaded     map[string]string
}

func newCountMetrics() *countMetrics                    { return &countMetrics{loaded: map[string]string{}} }
func (m *countMetrics) IncModelLoadError()              { m.loadErrors++ }
func (m *countMetrics) SetLoadedModel(kind, ver string) { m.loaded[kind] = ver }

func strp(s string) *string { return &s }

// TestLoaderNoActiveModelUsesHeuristic: with no active ranker, RankerFor always
// yields the heuristic.
func TestLoaderNoActiveModelUsesHeuristic(t *testing.T) {
	l := NewLoader(t.TempDir(), fakeLoaderQ{err: pgx.ErrNoRows}, 0.15, newCountMetrics(), nil)
	if err := l.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if learned, _ := l.Learned(); learned != nil {
		t.Fatalf("no active model should leave learned nil")
	}
	r, ver := l.RankerFor("anything")
	if ver != HeuristicVersion {
		t.Fatalf("want heuristic, got %q", ver)
	}
	if _, ok := r.(*Heuristic); !ok {
		t.Fatalf("RankerFor should return the heuristic")
	}
}

// TestLoaderBadArtifactKeepsHeuristic: an active row whose artifact is missing/
// corrupt must NOT error the refresh, must keep the heuristic, and must count a
// load error — the models row is never modified by the loader.
func TestLoaderBadArtifactKeepsHeuristic(t *testing.T) {
	metrics := newCountMetrics()
	l := NewLoader(t.TempDir(), fakeLoaderQ{active: sqlcgen.SearchModel{
		Kind: "ranker", Version: "ranker-bad", Status: "active",
		ArtifactPath: strp("does-not-exist.txt"), ArtifactSha256: strp("deadbeef"),
	}}, 0.15, metrics, nil)

	if err := l.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh must not error on a bad artifact: %v", err)
	}
	if learned, _ := l.Learned(); learned != nil {
		t.Fatalf("bad artifact must leave learned nil (heuristic)")
	}
	if metrics.loadErrors != 1 {
		t.Fatalf("expected 1 load error metric, got %d", metrics.loadErrors)
	}
	_, ver := l.RankerFor("ranker-bad")
	if ver != HeuristicVersion {
		t.Fatalf("bad artifact must fall back to heuristic, got %q", ver)
	}
}

// TestLoaderLoadsValidArtifactAndRoutes: a valid active artifact is loaded and
// served only when the requested version matches.
func TestLoaderLoadsValidArtifactAndRoutes(t *testing.T) {
	sum, err := fileSHA256(fixturePath())
	if err != nil {
		t.Fatalf("sha: %v", err)
	}
	abs, err := filepath.Abs(fixturePath())
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	metrics := newCountMetrics()
	l := NewLoader(t.TempDir(), fakeLoaderQ{active: sqlcgen.SearchModel{
		Kind: "ranker", Version: "ranker-v1", Status: "active",
		ArtifactPath: strp(abs), ArtifactSha256: strp(sum),
	}}, 0.15, metrics, nil)

	if err := l.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	learned, ver := l.Learned()
	if learned == nil || ver != "ranker-v1" {
		t.Fatalf("expected loaded ranker-v1, got %v/%q", learned, ver)
	}
	if metrics.loaded["ranker"] != "ranker-v1" {
		t.Fatalf("loaded gauge = %q, want ranker-v1", metrics.loaded["ranker"])
	}

	// Routes to the learned model when the version matches.
	r, rv := l.RankerFor("ranker-v1")
	if rv != "ranker-v1" {
		t.Fatalf("RankerFor(ranker-v1) = %q, want ranker-v1", rv)
	}
	if _, ok := r.(*Learned); !ok {
		t.Fatalf("expected the learned ranker")
	}
	// Falls back to heuristic when the routed version does not match.
	if _, rv := l.RankerFor("other-version"); rv != HeuristicVersion {
		t.Fatalf("unmatched version must fall back to heuristic, got %q", rv)
	}
	if _, rv := l.RankerFor(""); rv != HeuristicVersion {
		t.Fatalf("empty want-version must be heuristic, got %q", rv)
	}
}

// TestRerankFallbackEmptyDocs: rerankers handle an empty candidate set.
func TestRerankFallbackEmptyDocs(t *testing.T) {
	if got := NewHeuristic().Rerank(nil); got != nil {
		t.Errorf("heuristic empty rerank = %v, want nil", got)
	}
	m, err := LoadLeaves(fixturePath(), "")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := NewLearned(m, "v", 0.15).Rerank([]ranking.Doc{}); got != nil {
		t.Errorf("learned empty rerank = %v, want nil", got)
	}
}
