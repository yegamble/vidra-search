package experiment

import (
	"context"
	"encoding/json"
	"math"
	"testing"

	"github.com/vidra/vidra-search/internal/store/sqlcgen"
)

func TestBucketStable(t *testing.T) {
	b := Bucket("salt-x", "user-123")
	for i := 0; i < 100; i++ {
		if got := Bucket("salt-x", "user-123"); got != b {
			t.Fatalf("bucket not stable: %d vs %d", got, b)
		}
	}
	if b < 0 || b >= 100 {
		t.Fatalf("bucket %d out of [0,100)", b)
	}
	// The NUL separator prevents (salt,subject) concatenation collisions.
	if Bucket("ab", "c") == Bucket("a", "bc") {
		t.Errorf("salt/subject boundary collision")
	}
}

func TestBucketDistribution(t *testing.T) {
	counts := make([]int, 100)
	const n = 100000
	for i := 0; i < n; i++ {
		counts[Bucket("s", subjID(i))]++
	}
	// Chi-square-ish: no bucket should stray far from n/100.
	expected := float64(n) / 100
	for b, c := range counts {
		if math.Abs(float64(c)-expected)/expected > 0.20 {
			t.Errorf("bucket %d count %d strays >20%% from expected %.0f", b, c, expected)
		}
	}
}

func TestAssignVariantAndEmptySubject(t *testing.T) {
	reg := NewRegistry(fakeQ{exps: []sqlcgen.SearchExperiment{{
		Key:     "search_ranker",
		Salt:    "s1",
		Enabled: true,
		Variants: mustJSON(t, []Variant{
			{Name: "control", Min: 0, Max: 50, ModelVersion: "heuristic-v1"},
			{Name: "learned", Min: 50, Max: 100, ModelVersion: "ranker-v1"},
		}),
	}}}, nil)
	if err := reg.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	// Empty subject → no assignment.
	if _, ok := reg.Assign("search_ranker", ""); ok {
		t.Errorf("empty subject must not be assigned")
	}
	// Unknown experiment → no assignment.
	if _, ok := reg.Assign("nope", "user-1"); ok {
		t.Errorf("unknown experiment must not be assigned")
	}
	// A known subject → a stable, valid variant whose bucket is in range.
	a, ok := reg.Assign("search_ranker", "user-1")
	if !ok {
		t.Fatalf("expected an assignment")
	}
	if a.Variant != "control" && a.Variant != "learned" {
		t.Fatalf("unexpected variant %q", a.Variant)
	}
	if a.Bucket < 0 || a.Bucket >= 100 {
		t.Fatalf("bucket %d out of range", a.Bucket)
	}
	// Stability across calls.
	a2, _ := reg.Assign("search_ranker", "user-1")
	if a2 != a {
		t.Fatalf("assignment not stable: %+v vs %+v", a2, a)
	}
	// Model version routing matches the variant range.
	if a.Bucket < 50 && a.ModelVersion != "heuristic-v1" {
		t.Errorf("low bucket should route to heuristic-v1, got %q", a.ModelVersion)
	}
	if a.Bucket >= 50 && a.ModelVersion != "ranker-v1" {
		t.Errorf("high bucket should route to ranker-v1, got %q", a.ModelVersion)
	}
}

func TestRefreshFailStaticOnError(t *testing.T) {
	reg := NewRegistry(fakeQ{exps: []sqlcgen.SearchExperiment{{
		Key: "e", Salt: "s", Enabled: true,
		Variants: mustJSON(t, []Variant{{Name: "v", Min: 0, Max: 100}}),
	}}}, nil)
	if err := reg.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if _, ok := reg.Assign("e", "u"); !ok {
		t.Fatalf("expected assignment after good refresh")
	}
	// A failing refresh keeps the previous cache (fail-static).
	reg.q = fakeQ{err: errBoom}
	if err := reg.Refresh(context.Background()); err == nil {
		t.Fatalf("expected refresh error")
	}
	if _, ok := reg.Assign("e", "u"); !ok {
		t.Errorf("previous cache must survive a failed refresh")
	}
}

// --- fakes ---

type fakeQ struct {
	exps []sqlcgen.SearchExperiment
	err  error
}

func (f fakeQ) ListEnabledExperiments(context.Context) ([]sqlcgen.SearchExperiment, error) {
	return f.exps, f.err
}

var errBoom = errBoomType("boom")

type errBoomType string

func (e errBoomType) Error() string { return string(e) }

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func subjID(i int) string {
	return "subject-" + itoa(i)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}
