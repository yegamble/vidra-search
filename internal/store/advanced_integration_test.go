//go:build integration

// W3 integration tests: co-visitation rollup → item_neighbors → advanced
// recommendations, the model-registry lifecycle against the REAL committed
// LightGBM artifact, and shadow evaluation over synthetic impressions. They run
// against the same live Postgres + Redis as the W1/W2 suites and self-skip when
// DATABASE_URL/REDIS_URL are unset.
package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vidra/vidra-search/internal/event"
	"github.com/vidra/vidra-search/internal/experiment"
	"github.com/vidra/vidra-search/internal/recommendation"
	"github.com/vidra/vidra-search/internal/search"
	"github.com/vidra/vidra-search/internal/store/sqlcgen"
)

// setMode writes the instance search_mode into service_config.
func setMode(t *testing.T, env *testEnv, mode string) {
	t.Helper()
	if err := env.store.Queries().UpsertServiceConfig(context.Background(),
		sqlcgen.UpsertServiceConfigParams{Key: "search_mode", Value: mode}); err != nil {
		t.Fatalf("set mode: %v", err)
	}
}

func hasReason(items []recommendation.Item, reason string) bool {
	for _, it := range items {
		if it.Reason == reason {
			return true
		}
	}
	return false
}

// TestIntegrationCovisRollupToAdvancedRelated drives co-visitation end to end:
// synthetic session plays → co_watch → item_neighbors, then proves advanced
// related surfaces the co-watched videos (reason co_watch) where simple mode does
// not.
func TestIntegrationCovisRollupToAdvancedRelated(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	now := time.Now()

	// Four distinct-channel videos with no shared tags, so ONLY co-visitation can
	// relate them (simple related has no same-channel/overlap signal).
	va := video("alpha unique topic")
	vb := video("bravo unrelated subject")
	vc := video("charlie different theme")
	vd := video("delta popular filler", func(v *event.VideoDoc) { v.Views = 100000 })
	ingest(t, env, upsertEnvelope(t, va), upsertEnvelope(t, vb), upsertEnvelope(t, vc), upsertEnvelope(t, vd))

	// Two sessions co-watch (va, vb, vc) so the pairs clear a little support.
	for _, sess := range []string{"cw-s1", "cw-s2"} {
		u := uuid.New()
		ingest(t, env,
			play(now, va.ID, "", &u, sess, false),
			play(now.Add(time.Second), vb.ID, "", &u, sess, false),
			play(now.Add(2*time.Second), vc.ID, "", &u, sess, false),
		)
	}

	runWorker(t, env, "covis_rollup")

	// co_watch + item_neighbors are populated.
	if n := countRows(t, env, "SELECT count(*) FROM search.co_watch"); n == 0 {
		t.Fatalf("expected co_watch pairs, got 0")
	}
	if n := countRows(t, env, "SELECT count(*) FROM search.item_neighbors WHERE video_id = $1", va.ID); n == 0 {
		t.Fatalf("expected item_neighbors for va, got 0")
	}

	// Simple related(va): no co_watch reason (distinct channels, no overlap).
	simple, err := env.rec.Related(ctx, recommendation.RelatedRequest{VideoID: va.ID, Limit: 10})
	if err != nil {
		t.Fatalf("simple related: %v", err)
	}
	if hasReason(simple.Items, recommendation.ReasonCoWatch) {
		t.Errorf("simple related must not carry a co_watch reason: %+v", simple.Items)
	}

	// Advanced related(va): surfaces the co-watched vb/vc with reason co_watch.
	setMode(t, env, "advanced")
	adv, err := env.rec.Related(ctx, recommendation.RelatedRequest{VideoID: va.ID, Limit: 10})
	if err != nil {
		t.Fatalf("advanced related: %v", err)
	}
	if adv.ModelVersion != recommendation.AdvancedModelVersion {
		t.Errorf("advanced related model_version = %q, want %q", adv.ModelVersion, recommendation.AdvancedModelVersion)
	}
	if !hasReason(adv.Items, recommendation.ReasonCoWatch) {
		t.Errorf("advanced related must surface co-watched neighbors (co_watch reason): %+v", adv.Items)
	}
	if containsItem(adv.Items, va.ID) {
		t.Errorf("advanced related must exclude the seed")
	}

	// Determinism: identical inputs → identical order.
	adv2, _ := env.rec.Related(ctx, recommendation.RelatedRequest{VideoID: va.ID, Limit: 10})
	if !sameItemOrder(adv.Items, adv2.Items) {
		t.Errorf("advanced related not deterministic")
	}
}

// copyFixtureModel copies the committed tiny LightGBM artifact into the test model
// dir as ranker-<version>.txt and returns its (basename, sha256).
func copyFixtureModel(t *testing.T, env *testEnv, version string) (string, string) {
	t.Helper()
	src := filepath.Join("..", "model", "testdata", "tiny-ranker.txt")
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read fixture model: %v", err)
	}
	base := "ranker-" + version + ".txt"
	if err := os.WriteFile(filepath.Join(env.modelDir, base), data, 0o600); err != nil {
		t.Fatalf("write model: %v", err)
	}
	sum := sha256.Sum256(data)
	return base, hex.EncodeToString(sum[:])
}

// TestIntegrationModelRegistryLifecycle drives a ranker through the registry:
// shadow insert → activate (retiring nothing) → loader hot-swap → serve → and the
// activate/retire status transitions.
func TestIntegrationModelRegistryLifecycle(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	q := env.store.Queries()

	base, sha := copyFixtureModel(t, env, "v1")
	if _, err := q.InsertModel(ctx, sqlcgen.InsertModelParams{
		Kind: "ranker", Version: "v1", Status: "shadow",
		ArtifactSha256: &sha, ArtifactPath: &base, Metrics: []byte(`{}`),
	}); err != nil {
		t.Fatalf("insert shadow model: %v", err)
	}

	// Shadow is not served: loader stays on the heuristic.
	if err := env.loader.Refresh(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if learned, _ := env.loader.Learned(); learned != nil {
		t.Fatalf("a shadow model must not be loaded for serving")
	}

	// Activate → loader hot-swaps to the learned model (via the model_loader job).
	if err := q.ActivateModel(ctx, sqlcgen.ActivateModelParams{Kind: "ranker", Version: "v1"}); err != nil {
		t.Fatalf("activate: %v", err)
	}
	runWorker(t, env, "model_loader")
	learned, ver := env.loader.Learned()
	if learned == nil || ver != "v1" {
		t.Fatalf("expected loaded ranker v1, got %v/%q", learned, ver)
	}
	active, err := q.GetActiveModel(ctx, "ranker")
	if err != nil || active.Version != "v1" || active.Status != "active" || !active.ActivatedAt.Valid {
		t.Fatalf("active model row wrong: %+v err=%v", active, err)
	}

	// Roll a second version in and activate it, retiring v1 (make activate-model).
	base2, sha2 := copyFixtureModel(t, env, "v2")
	if _, err := q.InsertModel(ctx, sqlcgen.InsertModelParams{
		Kind: "ranker", Version: "v2", Status: "shadow",
		ArtifactSha256: &sha2, ArtifactPath: &base2, Metrics: []byte(`{}`),
	}); err != nil {
		t.Fatalf("insert v2: %v", err)
	}
	if err := q.RetireActiveModels(ctx, "ranker"); err != nil {
		t.Fatalf("retire: %v", err)
	}
	if err := q.ActivateModel(ctx, sqlcgen.ActivateModelParams{Kind: "ranker", Version: "v2"}); err != nil {
		t.Fatalf("activate v2: %v", err)
	}
	runWorker(t, env, "model_loader")
	if _, ver := env.loader.Learned(); ver != "v2" {
		t.Fatalf("loader should hot-swap to v2, got %q", ver)
	}
	if n := countRows(t, env, "SELECT count(*) FROM search.models WHERE version='v1' AND status='retired'"); n != 1 {
		t.Errorf("v1 must be retired after v2 activation")
	}
}

// TestIntegrationShadowEval scores a shadow ranker over synthetic impressions and
// writes the metrics blob to the models row, without activating it.
func TestIntegrationShadowEval(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	q := env.store.Queries()

	base, sha := copyFixtureModel(t, env, "shadow1")
	if _, err := q.InsertModel(ctx, sqlcgen.InsertModelParams{
		Kind: "ranker", Version: "shadow1", Status: "shadow",
		ArtifactSha256: &sha, ArtifactPath: &base, Metrics: []byte(`{}`),
	}); err != nil {
		t.Fatalf("insert shadow model: %v", err)
	}

	// A query with three impressed videos; the click lands on the LAST-served one
	// (a suboptimal production order the ranker can potentially improve on).
	now := time.Now()
	const qy = "kubernetes"
	v1 := video("kubernetes basics tutorial", func(v *event.VideoDoc) { v.Views = 10 })
	v2 := video("kubernetes networking deep dive", func(v *event.VideoDoc) { v.Views = 20 })
	v3 := video("kubernetes operators guide", func(v *event.VideoDoc) { v.Views = 5 })
	ingest(t, env, upsertEnvelope(t, v1), upsertEnvelope(t, v2), upsertEnvelope(t, v3))
	ingest(t, env,
		impression(now, qy, v1.ID, "sh-s1"),
		impression(now, qy, v2.ID, "sh-s1"),
		impression(now, qy, v3.ID, "sh-s1"),
		clicked(now.Add(time.Second), qy, v3.ID, nil, "sh-s1"),
	)

	// Run the shadow evaluation job.
	runWorker(t, env, "shadow_eval")

	m, err := q.GetModelByVersion(ctx, sqlcgen.GetModelByVersionParams{Kind: "ranker", Version: "shadow1"})
	if err != nil {
		t.Fatalf("get model: %v", err)
	}
	if m.Status != "shadow" {
		t.Errorf("shadow eval must NOT activate the model, status=%q", m.Status)
	}
	var rep struct {
		Groups         int     `json:"groups"`
		ShadowNDCG     float64 `json:"ndcg@10"`
		ProductionNDCG float64 `json:"production_ndcg@10"`
		HeuristicNDCG  float64 `json:"heuristic_ndcg@10"`
	}
	if err := json.Unmarshal(m.Metrics, &rep); err != nil {
		t.Fatalf("metrics json: %v (%s)", err, string(m.Metrics))
	}
	if rep.Groups < 1 {
		t.Errorf("shadow eval should have scored >=1 impression group, got %d (%s)", rep.Groups, string(m.Metrics))
	}
}

// TestIntegrationAdvancedSearchExperiment proves advanced search returns ranked
// ids and stamps the experiment assignment when one is defined.
func TestIntegrationAdvancedSearchExperiment(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	q := env.store.Queries()

	// Define + cache an experiment routing everyone to the heuristic variant.
	variants, _ := json.Marshal([]experiment.Variant{{Name: "control", Min: 0, Max: 100, ModelVersion: "heuristic-v1"}})
	if err := q.UpsertExperiment(ctx, sqlcgen.UpsertExperimentParams{
		Key: search.ExperimentKey, Description: "t", Variants: variants, Salt: "s1", Enabled: true,
	}); err != nil {
		t.Fatalf("upsert experiment: %v", err)
	}
	if err := env.experiments.Refresh(ctx); err != nil {
		t.Fatalf("refresh experiments: %v", err)
	}

	ingest(t, env,
		upsertEnvelope(t, video("golang concurrency patterns")),
		upsertEnvelope(t, video("golang generics tutorial")),
	)

	uid := uuid.New()
	resp, err := env.search.Search(ctx, search.Request{
		Query: "golang", Limit: 10, Mode: "advanced", UserID: uid.String(), Personalized: true,
	})
	if err != nil {
		t.Fatalf("advanced search: %v", err)
	}
	if len(resp.IDs) == 0 {
		t.Fatalf("advanced search returned no ids")
	}
	if resp.Experiment == nil || resp.Experiment.Experiment != search.ExperimentKey {
		t.Fatalf("advanced search must stamp the experiment assignment, got %+v", resp.Experiment)
	}
	if resp.ModelVersion == "" {
		t.Errorf("advanced search must report a model_version")
	}
}
