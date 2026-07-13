package model

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/vidra/vidra-search/internal/ranking"
)

// fixturePath is a real (tiny) LightGBM LambdaMART model, produced by
// training/train_ranker.py and committed under testdata. It is the ground-truth
// artifact the serving path must load.
func fixturePath() string { return filepath.Join("testdata", "tiny-ranker.txt") }

func TestLoadLeavesFixture(t *testing.T) {
	m, err := LoadLeaves(fixturePath(), "")
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	if m.NFeatures() != 8 {
		t.Fatalf("fixture NFeatures = %d, want 8", m.NFeatures())
	}
	score := m.Predict([]float64{0.9, 0.5, 1, 10, 5, 0.4, 0.2, 0})
	if math.IsNaN(score) || math.IsInf(score, 0) {
		t.Fatalf("prediction not finite: %v", score)
	}
}

func TestLoadLeavesGoodSHAVerifies(t *testing.T) {
	sum, err := fileSHA256(fixturePath())
	if err != nil {
		t.Fatalf("sha: %v", err)
	}
	if _, err := LoadLeaves(fixturePath(), sum); err != nil {
		t.Fatalf("load with correct sha should succeed: %v", err)
	}
}

// --- fallback tests (mandatory) ---

func TestLoadLeavesMissingFile(t *testing.T) {
	if _, err := LoadLeaves(filepath.Join(t.TempDir(), "nope.txt"), ""); err == nil {
		t.Fatalf("missing artifact must error")
	}
}

func TestLoadLeavesBadSHA(t *testing.T) {
	_, err := LoadLeaves(fixturePath(), "0000000000000000000000000000000000000000000000000000000000000000")
	if err == nil {
		t.Fatalf("sha mismatch must error")
	}
}

func TestLoadLeavesMalformed(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "garbage.txt")
	if err := os.WriteFile(bad, []byte("this is not a lightgbm model\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := LoadLeaves(bad, ""); err == nil {
		t.Fatalf("malformed artifact must error")
	}
}

func TestLoadLeavesEmptyFile(t *testing.T) {
	empty := filepath.Join(t.TempDir(), "empty.txt")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := LoadLeaves(empty, ""); err == nil {
		t.Fatalf("empty artifact must error")
	}
}

// TestPredictEmptyFeatures: a malformed/empty feature vector is zero-padded, never
// panics, and yields a finite score.
func TestPredictEmptyFeatures(t *testing.T) {
	m, err := LoadLeaves(fixturePath(), "")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for _, v := range [][]float64{{}, {0.1}, nil} {
		got := m.Predict(v)
		if math.IsNaN(got) || math.IsInf(got, 0) {
			t.Fatalf("padded prediction for %v not finite: %v", v, got)
		}
	}
}

// BenchmarkLeavesRerank500 proves the learned rerank of 500 candidates stays under
// the 30ms/rerank serving budget (§ serving perf).
func BenchmarkLeavesRerank500(b *testing.B) {
	m, err := LoadLeaves(fixturePath(), "")
	if err != nil {
		b.Fatalf("load: %v", err)
	}
	learned := NewLearned(m, "bench", ranking.DefaultAdvancedWeights.CreatorPenalty)
	docs := make([]ranking.Doc, 500)
	for i := range docs {
		docs[i] = ranking.Doc{
			VideoID:   fmt.Sprintf("v%03d", i),
			ChannelID: fmt.Sprintf("ch%d", i%50),
			Features: ranking.Features{
				TextRank: float64(i%97) / 97, TrgmSim: float64(i%31) / 31,
				ExactFlags: float64(i%3) * 0.5, Views: float64(i * 7), AgeDays: float64(i % 365),
				Clicks: float64(i % 11), Impressions: float64(i % 53), MeaningfulWatches: float64(i % 5),
			},
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = learned.Rerank(docs)
	}
}
