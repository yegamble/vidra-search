// Package experiment implements deterministic, low-overhead A/B assignment
// (§1.5). An enabled experiment buckets a subject by hash(salt + subject_id) %
// 100; the bucket falls in one variant's [min,max) range. Definitions are cached
// in RAM and refreshed periodically so assignment is a pure, allocation-light
// hash with no per-request database read. The chosen variant (and its
// model_version) is surfaced in search/recommendation responses and, via
// model_version, into the impression log — so offline analysis attributes
// outcomes to a variant without any server-side per-request event write.
package experiment

import (
	"context"
	"encoding/json"
	"hash/fnv"
	"log/slog"
	"sort"
	"sync"

	"github.com/vidra/vidra-search/internal/store/sqlcgen"
)

// Variant is one arm of an experiment occupying the half-open bucket range
// [Min,Max) out of 100. ModelVersion optionally routes this arm to a specific
// ranker (e.g. a learned model) and is the version stamped into responses/logs.
type Variant struct {
	Name         string `json:"name"`
	Min          int    `json:"min"`
	Max          int    `json:"max"`
	ModelVersion string `json:"model_version"`
}

// Experiment is a cached, enabled experiment definition.
type Experiment struct {
	Key      string
	Salt     string
	Variants []Variant
}

// Assignment is the result of assigning a subject to an experiment.
type Assignment struct {
	Experiment   string `json:"experiment"`
	Variant      string `json:"variant"`
	Bucket       int    `json:"bucket"`
	ModelVersion string `json:"model_version,omitempty"`
}

// Querier is the store surface the registry loads definitions from.
type Querier interface {
	ListEnabledExperiments(ctx context.Context) ([]sqlcgen.SearchExperiment, error)
}

// Registry is the in-RAM cache of enabled experiments with a refresh seam.
type Registry struct {
	q      Querier
	logger *slog.Logger
	mu     sync.RWMutex
	byKey  map[string]Experiment
}

// NewRegistry builds an empty registry. Call Refresh (or Start) to populate it.
func NewRegistry(q Querier, logger *slog.Logger) *Registry {
	if logger == nil {
		logger = slog.Default()
	}
	return &Registry{q: q, logger: logger, byKey: map[string]Experiment{}}
}

// Refresh reloads the enabled experiment definitions from the store. A load error
// leaves the previous cache in place (fail-static) and is returned for logging.
func (r *Registry) Refresh(ctx context.Context) error {
	rows, err := r.q.ListEnabledExperiments(ctx)
	if err != nil {
		return err
	}
	next := make(map[string]Experiment, len(rows))
	for _, row := range rows {
		var variants []Variant
		if len(row.Variants) > 0 {
			if err := json.Unmarshal(row.Variants, &variants); err != nil {
				r.logger.Warn("experiment: bad variants JSON, skipping", "key", row.Key, "error", err)
				continue
			}
		}
		// Sort variants by Min so assignment is order-independent of storage.
		sort.SliceStable(variants, func(i, j int) bool { return variants[i].Min < variants[j].Min })
		next[row.Key] = Experiment{Key: row.Key, Salt: row.Salt, Variants: variants}
	}
	r.mu.Lock()
	r.byKey = next
	r.mu.Unlock()
	return nil
}

// Assign buckets a subject into the experiment's variant. It returns (assignment,
// true) only when the experiment exists (is enabled + cached), the subject is
// non-empty, and the bucket falls in a defined variant. An empty subject (no
// user_id and no session_id) is never assigned — such requests carry no
// experiment (§1.5).
func (r *Registry) Assign(key, subject string) (Assignment, bool) {
	if subject == "" {
		return Assignment{}, false
	}
	r.mu.RLock()
	exp, ok := r.byKey[key]
	r.mu.RUnlock()
	if !ok {
		return Assignment{}, false
	}
	bucket := Bucket(exp.Salt, subject)
	for _, v := range exp.Variants {
		if bucket >= v.Min && bucket < v.Max {
			return Assignment{
				Experiment:   key,
				Variant:      v.Name,
				Bucket:       bucket,
				ModelVersion: v.ModelVersion,
			}, true
		}
	}
	return Assignment{}, false
}

// Bucket is the deterministic assignment hash: fnv1a(salt + "\x00" + subject) %
// 100. The NUL separator prevents (salt="a", subject="bc") colliding with
// (salt="ab", subject="c"). Stable across processes and restarts.
func Bucket(salt, subject string) int {
	h := fnv.New64a()
	_, _ = h.Write([]byte(salt))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(subject))
	return int(h.Sum64() % 100)
}
