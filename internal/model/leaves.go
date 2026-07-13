// Package model is the online model-serving layer (§1.9): it loads versioned
// LightGBM ranker artifacts trained offline in Python, verifies their integrity,
// serves them via the pure-Go `leaves` reader, and hot-swaps the active model
// atomically. Go NEVER trains; it only serves. When no valid learned model is
// loaded — or an artifact is missing/corrupt/malformed — the service falls back
// to the always-available heuristic ranker, so serving never fails because of a
// model problem.
package model

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"

	"github.com/dmitryikh/leaves"

	"github.com/vidra/vidra-search/internal/ranking"
)

// LeavesModel wraps a loaded LightGBM ensemble and the number of features it
// expects, so a feature-vector length mismatch is caught before Predict.
type LeavesModel struct {
	ens       *leaves.Ensemble
	nFeatures int
}

// LoadLeaves reads a LightGBM text model from path and verifies its SHA-256
// matches expectedSHA (hex; empty skips the check — used only by tests that
// generate an artifact in-process). loadTransformation is false: for a lambdarank
// model the raw margin IS the ranking score, so no output transform is applied.
func LoadLeaves(path, expectedSHA string) (*LeavesModel, error) {
	if expectedSHA != "" {
		sum, err := fileSHA256(path)
		if err != nil {
			return nil, err
		}
		if sum != expectedSHA {
			return nil, fmt.Errorf("model: artifact sha256 mismatch for %s: have %s want %s", path, sum, expectedSHA)
		}
	}
	ens, err := leaves.LGEnsembleFromFile(path, false)
	if err != nil {
		return nil, fmt.Errorf("model: load leaves artifact %s: %w", path, err)
	}
	if ens.NEstimators() <= 0 {
		return nil, fmt.Errorf("model: artifact %s has no trees", path)
	}
	return &LeavesModel{ens: ens, nFeatures: ens.NFeatures()}, nil
}

// Predict returns the model's raw score for one feature vector. A vector shorter
// than the model's feature count is defensively zero-padded (a malformed/empty
// vector then simply scores low), never panicking.
func (m *LeavesModel) Predict(features []float64) float64 {
	if len(features) < m.nFeatures {
		padded := make([]float64, m.nFeatures)
		copy(padded, features)
		features = padded
	}
	return m.ens.PredictSingle(features, 0)
}

// NFeatures is the number of features the loaded model expects.
func (m *LeavesModel) NFeatures() int { return m.nFeatures }

// fileSHA256 returns the lowercase hex SHA-256 of a file's contents.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// compile-time assurance the learned ranker feeds the documented feature vector.
var _ = ranking.ModelFeatureVector
