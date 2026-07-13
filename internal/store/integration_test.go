//go:build integration

// Integration tests for vidra-search. They exercise the REAL sqlc SQL against a
// live Postgres (with pg_trgm + the `search` schema) and a live Redis, driving
// the actual service layer end to end. Each test self-skips when DATABASE_URL /
// REDIS_URL are unset, so `make test-integration` is safe on a bare host.
//
// Run locally:
//
//	docker compose up -d postgres redis migrate
//	export DATABASE_URL=postgres://vidra_search:vidra_search@localhost:5433/vidra_search?sslmode=disable
//	export REDIS_URL=redis://localhost:6380/0
//	go test -tags integration ./...
package store_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vidra/vidra-search/internal/cache"
	"github.com/vidra/vidra-search/internal/event"
	"github.com/vidra/vidra-search/internal/experiment"
	"github.com/vidra/vidra-search/internal/history"
	"github.com/vidra/vidra-search/internal/model"
	"github.com/vidra/vidra-search/internal/ranking"
	"github.com/vidra/vidra-search/internal/recommendation"
	"github.com/vidra/vidra-search/internal/search"
	"github.com/vidra/vidra-search/internal/store"
	"github.com/vidra/vidra-search/internal/suggest"
	"github.com/vidra/vidra-search/internal/worker"
)

// testEnv bundles the live dependencies and services for a test.
type testEnv struct {
	store       *store.Store
	cache       *cache.Cache
	events      *event.Service
	sugg        *suggest.Service
	search      *search.Service
	rec         *recommendation.Service
	history     *history.Service
	worker      *worker.Runner
	loader      *model.Loader
	experiments *experiment.Registry
	evaluator   *model.ShadowEvaluator
	modelDir    string
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	redisURL := os.Getenv("REDIS_URL")
	if dsn == "" || redisURL == "" {
		t.Skip("integration test: DATABASE_URL and REDIS_URL must be set")
	}
	ctx := context.Background()
	st, err := store.New(ctx, dsn)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(st.Close)

	rdb, err := cache.New(ctx, redisURL)
	if err != nil {
		t.Fatalf("cache.New: %v", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })

	// Clean slate for every test — Postgres tables and the isolated test Redis.
	if _, err := st.Pool.Exec(ctx, `TRUNCATE
		search.documents, search.events_inbox, search.service_config,
		search.query_log, search.query_aggregates, search.user_search_history,
		search.user_watch_projection, search.behavior_events,
		search.query_video_engagement, search.worker_cursors,
		search.co_watch, search.co_search, search.item_neighbors,
		search.models, search.experiments RESTART IDENTITY`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if err := rdb.Client.FlushDB(ctx).Err(); err != nil {
		t.Fatalf("flushdb: %v", err)
	}

	q := st.Queries()
	// A per-user trend cap window that easily spans a test run so repeated bumps
	// from one subject collapse to a single ranking contribution.
	eventCfg := event.Config{TrendCapWindow: time.Hour, TrendingHalfLifeSeconds: 6 * 3600, WatchHalfLifeHours: 720}
	workerCfg := worker.Config{MinQueryUserCount: 3, TrendCapWindow: time.Hour, WilsonFloor: 0.10}

	modelDir := t.TempDir()
	loader := model.NewLoader(modelDir, q, ranking.DefaultAdvancedWeights.CreatorPenalty, nil, nil)
	experiments := experiment.NewRegistry(q, nil)
	evaluator := model.NewShadowEvaluator(q, loader, nil, nil, 30)

	runner := worker.NewRunner(st, rdb, workerCfg, nil, nil)
	runner.AddJob(worker.PeriodicJob{Name: "model_loader", Run: loader.Refresh})
	runner.AddJob(worker.PeriodicJob{Name: "experiment_refresh", Run: experiments.Refresh})
	runner.AddJob(worker.PeriodicJob{Name: "shadow_eval", Run: evaluator.Run})

	return &testEnv{
		store:       st,
		cache:       rdb,
		events:      event.NewService(st, nil, nil, eventCfg, rdb),
		sugg:        suggest.NewService(q, suggest.NewStoreAggregate(q), rdb, rdb, rdb, nil),
		search:      search.NewService(q, loader, experiments, rdb, nil),
		rec:         recommendation.NewService(q, rdb, loader, experiments, rdb, nil),
		history:     history.NewService(st),
		worker:      runner,
		loader:      loader,
		experiments: experiments,
		evaluator:   evaluator,
		modelDir:    modelDir,
	}
}

// --- envelope builders ---

func ptr[T any](v T) *T { return &v }

func upsertEnvelope(t *testing.T, v event.VideoDoc) event.Envelope {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"video": v})
	if err != nil {
		t.Fatalf("marshal video: %v", err)
	}
	return event.Envelope{EventID: uuid.New(), Type: event.TypeVideoUpsert, OccurredAt: time.Now(), SchemaVersion: 1, Payload: raw}
}

func rawEnvelope(t *testing.T, typ string, payload any) event.Envelope {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal %s: %v", typ, err)
	}
	return event.Envelope{EventID: uuid.New(), Type: typ, OccurredAt: time.Now(), SchemaVersion: 1, Payload: raw}
}

// video builds a public, published VideoDoc with sane defaults.
func video(title string, opts ...func(*event.VideoDoc)) event.VideoDoc {
	now := time.Now().UTC()
	v := event.VideoDoc{
		ID:          uuid.New(),
		Kind:        "local",
		ChannelID:   ptr(uuid.New()),
		Title:       title,
		Tags:        []string{},
		Privacy:     "public",
		State:       "published",
		PublishedAt: ptr(now),
		CreatedAt:   ptr(now),
		UpdatedAt:   ptr(now),
	}
	for _, o := range opts {
		o(&v)
	}
	return v
}

func ingest(t *testing.T, env *testEnv, evs ...event.Envelope) event.Result {
	t.Helper()
	res, err := env.events.Ingest(context.Background(), evs)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	return res
}

// --- tests ---

func TestIntegrationDocumentUpsertEligibility(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	pub := video("Public Published Video")
	priv := video("Private Video", func(v *event.VideoDoc) { v.Privacy = "private" })
	draft := video("Draft Video", func(v *event.VideoDoc) { v.State = "draft" })

	res := ingest(t, env, upsertEnvelope(t, pub), upsertEnvelope(t, priv), upsertEnvelope(t, draft))
	if res.Accepted != 3 || res.Duplicates != 0 || len(res.Failed) != 0 {
		t.Fatalf("unexpected ingest result: %+v", res)
	}

	q := env.store.Queries()
	got, err := q.GetDocument(ctx, pub.ID)
	if err != nil {
		t.Fatalf("get pub: %v", err)
	}
	if !got.Eligible {
		t.Errorf("public/published must be eligible")
	}
	gotPriv, _ := q.GetDocument(ctx, priv.ID)
	if gotPriv.Eligible || gotPriv.SuppressedReason == nil || *gotPriv.SuppressedReason != "privacy_private" {
		t.Errorf("private must be ineligible with reason privacy_private, got eligible=%v reason=%v", gotPriv.Eligible, gotPriv.SuppressedReason)
	}
	gotDraft, _ := q.GetDocument(ctx, draft.ID)
	if gotDraft.Eligible || gotDraft.SuppressedReason == nil || *gotDraft.SuppressedReason != "state_draft" {
		t.Errorf("draft must be ineligible with reason state_draft, got eligible=%v reason=%v", gotDraft.Eligible, gotDraft.SuppressedReason)
	}
}

// TestIntegrationOwnerUnlistedPreservesOtherSuppression is the W1-audit fix (#1):
// suppressing an owner's videos as unlisted must NOT overwrite a stronger
// suppression reason, and relisting must not resurrect a genuinely deleted video.
func TestIntegrationOwnerUnlistedPreservesOtherSuppression(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	owner := uuid.New()
	v := video("Deleted Then Owner Toggled", func(d *event.VideoDoc) { d.OwnerID = ptr(owner) })
	ingest(t, env, upsertEnvelope(t, v))

	// Hard-delete the video (a stronger reason than owner_unlisted).
	ingest(t, env, rawEnvelope(t, event.TypeVideoSuppress, map[string]any{"video_id": v.ID, "reason": "deleted"}))

	// Owner goes unlisted, then relists. The unlisted suppress must skip the
	// already-ineligible (deleted) doc, so the relist restore cannot re-enable it.
	ingest(t, env, rawEnvelope(t, event.TypeUserSuppress, map[string]any{"user_id": owner, "unlisted": true}))
	ingest(t, env, rawEnvelope(t, event.TypeUserSuppress, map[string]any{"user_id": owner, "unlisted": false}))

	got, err := env.store.Queries().GetDocument(ctx, v.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Eligible {
		t.Errorf("deleted video must stay ineligible through an unlist/relist cycle")
	}
	if got.SuppressedReason == nil || *got.SuppressedReason != "deleted" {
		t.Errorf("suppression reason must remain 'deleted', got %v", got.SuppressedReason)
	}
}

func TestIntegrationEventIdempotency(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	v := video("Idempotent Video", func(d *event.VideoDoc) { d.Views = 42 })
	batch := []event.Envelope{upsertEnvelope(t, v)}

	first := ingest(t, env, batch...)
	if first.Accepted != 1 || first.Duplicates != 0 {
		t.Fatalf("first ingest: %+v", first)
	}
	before, _ := env.store.Queries().GetDocument(ctx, v.ID)

	// Replaying the identical batch must be a pure no-op: all duplicates.
	second := ingest(t, env, batch...)
	if second.Accepted != 0 || second.Duplicates != 1 || len(second.Failed) != 0 {
		t.Fatalf("replay must be all-duplicate, got %+v", second)
	}
	after, _ := env.store.Queries().GetDocument(ctx, v.ID)
	if before.Views != after.Views || before.IndexedAt != after.IndexedAt {
		t.Errorf("state changed on replay: before=%v/%v after=%v/%v", before.Views, before.IndexedAt, after.Views, after.IndexedAt)
	}
}

func TestIntegrationSuggestions(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	ingest(t, env,
		upsertEnvelope(t, video("golang tutorial", func(v *event.VideoDoc) { v.Views = 100 })),
		upsertEnvelope(t, video("golang basics", func(v *event.VideoDoc) { v.Views = 500 })),
		upsertEnvelope(t, video("cooking pasta", func(v *event.VideoDoc) { v.Views = 9000 })),
	)

	resp := env.sugg.Suggest(ctx, suggest.Request{Query: "golang", Limit: 10})
	if len(resp.Suggestions) < 2 {
		t.Fatalf("expected >=2 suggestions, got %v", resp.Suggestions)
	}
	// Both golang titles are exact-prefix; the higher-view one ranks first.
	if resp.Suggestions[0].Text != "golang basics" || resp.Suggestions[1].Text != "golang tutorial" {
		t.Errorf("unexpected suggestion order: %v", suggestionTexts(resp.Suggestions))
	}
	if resp.NormalizedQuery != "golang" || resp.ModelVersion != "heuristic-v1" {
		t.Errorf("unexpected envelope: %+v", resp)
	}
}

func TestIntegrationSearchFiltersAndSensitive(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	clean := video("golang concurrency guide", func(v *event.VideoDoc) {
		v.Tags = []string{"programming", "go"}
		v.Category = ptr("education")
		v.Language = ptr("en")
	})
	sensitive := video("golang concurrency uncensored", func(v *event.VideoDoc) {
		v.IsSensitive = true
		v.Tags = []string{"programming"}
	})
	other := video("golang concurrency spanish", func(v *event.VideoDoc) {
		v.Language = ptr("es")
		v.Tags = []string{"programming"}
	})
	ingest(t, env, upsertEnvelope(t, clean), upsertEnvelope(t, sensitive), upsertEnvelope(t, other))

	// hide_sensitive drops the sensitive doc.
	resp, err := env.search.Search(ctx, search.Request{Query: "golang concurrency", Limit: 50, HideSensitive: true})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if containsID(resp.IDs, sensitive.ID) {
		t.Errorf("hide_sensitive must exclude the sensitive doc")
	}
	if !containsID(resp.IDs, clean.ID) {
		t.Errorf("expected the clean doc in results")
	}

	// Language filter keeps only es.
	respEs, _ := env.search.Search(ctx, search.Request{Query: "golang concurrency", Limit: 50, Language: "es"})
	if !containsID(respEs.IDs, other.ID) || containsID(respEs.IDs, clean.ID) {
		t.Errorf("language filter failed: %+v", respEs.IDs)
	}

	// Tag filter.
	respTag, _ := env.search.Search(ctx, search.Request{Query: "golang concurrency", Limit: 50, Tag: "go", HideSensitive: false})
	if !containsID(respTag.IDs, clean.ID) || containsID(respTag.IDs, other.ID) {
		t.Errorf("tag filter failed: %+v", respTag.IDs)
	}
}

func TestIntegrationRelatedAndHomeDeterminism(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	ch := uuid.New()
	seed := video("seed video", func(v *event.VideoDoc) {
		v.ChannelID = ptr(ch)
		v.Tags = []string{"music", "live"}
		v.Category = ptr("music")
		v.Language = ptr("en")
	})
	sameCh := video("same channel newer", func(v *event.VideoDoc) { v.ChannelID = ptr(ch) })
	overlap := video("overlapping tags", func(v *event.VideoDoc) { v.Tags = []string{"music"} })
	popular := video("popular unrelated", func(v *event.VideoDoc) { v.Views = 100000 })
	ingest(t, env, upsertEnvelope(t, seed), upsertEnvelope(t, sameCh), upsertEnvelope(t, overlap), upsertEnvelope(t, popular))

	related, err := env.rec.Related(ctx, recommendation.RelatedRequest{VideoID: seed.ID, Limit: 10})
	if err != nil {
		t.Fatalf("related: %v", err)
	}
	if containsItem(related.Items, seed.ID) {
		t.Errorf("related must exclude the seed itself")
	}
	if len(related.Items) == 0 {
		t.Fatalf("expected related items")
	}
	// Determinism: identical inputs → identical output order.
	related2, _ := env.rec.Related(ctx, recommendation.RelatedRequest{VideoID: seed.ID, Limit: 10})
	if !sameItemOrder(related.Items, related2.Items) {
		t.Errorf("related not deterministic")
	}

	home, err := env.rec.Home(ctx, recommendation.HomeRequest{Limit: 10, Lang: "en"})
	if err != nil {
		t.Fatalf("home: %v", err)
	}
	if len(home.Items) == 0 {
		t.Fatalf("expected home items")
	}
	home2, _ := env.rec.Home(ctx, recommendation.HomeRequest{Limit: 10, Lang: "en"})
	if !sameItemOrder(home.Items, home2.Items) {
		t.Errorf("home not deterministic")
	}
	for _, it := range home.Items {
		switch it.Reason {
		case recommendation.ReasonTrending, recommendation.ReasonFresh, recommendation.ReasonPopular:
		default:
			t.Errorf("unexpected home reason %q", it.Reason)
		}
	}
}

func TestIntegrationReconcileOrphanSuppression(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	keep := video("kept video")
	orphan := video("orphaned video")
	ingest(t, env, upsertEnvelope(t, keep), upsertEnvelope(t, orphan))

	runID := uuid.New()
	// A reconcile pass that only re-sends `keep` must suppress `orphan` on end.
	keepUpdated := keep
	ingest(t, env,
		rawEnvelope(t, event.TypeReconcileBegin, map[string]any{"run_id": runID}),
		rawEnvelope(t, event.TypeReconcilePage, map[string]any{"run_id": runID, "videos": []event.VideoDoc{keepUpdated}}),
		rawEnvelope(t, event.TypeReconcileEnd, map[string]any{"run_id": runID, "total": 1}),
	)

	q := env.store.Queries()
	gotKeep, _ := q.GetDocument(ctx, keep.ID)
	if !gotKeep.Eligible {
		t.Errorf("reconciled doc must stay eligible")
	}
	gotOrphan, _ := q.GetDocument(ctx, orphan.ID)
	if gotOrphan.Eligible || gotOrphan.SuppressedReason == nil || *gotOrphan.SuppressedReason != "reconcile_orphan" {
		t.Errorf("orphan must be suppressed with reason reconcile_orphan, got eligible=%v reason=%v", gotOrphan.Eligible, gotOrphan.SuppressedReason)
	}
}

func TestIntegrationConfigUpdate(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	ingest(t, env, rawEnvelope(t, event.TypeConfigUpdated, map[string]any{
		"settings": map[string]any{
			"search_mode":              "advanced",
			"minimum_query_user_count": 5,
			"suggestions_enabled":      true,
		},
	}))
	rows, err := env.store.Queries().ListServiceConfig(ctx)
	if err != nil {
		t.Fatalf("list config: %v", err)
	}
	got := map[string]string{}
	for _, r := range rows {
		got[r.Key] = r.Value
	}
	if got["search_mode"] != "advanced" || got["minimum_query_user_count"] != "5" || got["suggestions_enabled"] != "true" {
		t.Errorf("unexpected config: %+v", got)
	}
}

// --- helpers ---

func suggestionTexts(s []ranking.Suggestion) []string {
	out := make([]string, len(s))
	for i, x := range s {
		out[i] = x.Text
	}
	return out
}

func containsID(hits []search.Hit, id uuid.UUID) bool {
	for _, h := range hits {
		if h.VideoID == id.String() {
			return true
		}
	}
	return false
}

func containsItem(items []recommendation.Item, id uuid.UUID) bool {
	for _, it := range items {
		if it.VideoID == id.String() {
			return true
		}
	}
	return false
}

func sameItemOrder(a, b []recommendation.Item) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].VideoID != b[i].VideoID {
			return false
		}
	}
	return true
}
