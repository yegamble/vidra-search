//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/vidra/vidra-search/internal/catalog"
	"github.com/vidra/vidra-search/internal/event"
)

func catalogFullEvents(t *testing.T, epoch uuid.UUID, generation catalog.Generation, v event.VideoDoc) []event.Envelope {
	t.Helper()
	id := catalog.Identity{Scope: "local", Epoch: epoch, Generation: generation}
	docs, _ := json.Marshal([]map[string]any{{"video": v, "eligible": true, "suppressed_reason": nil}})
	_, hash, _ := catalog.DocumentsDigest(docs)
	membership, _ := catalog.MembershipDigest([]uuid.UUID{v.ID})
	content, _ := catalog.PagesDigest([]catalog.PageDigest{{PageIndex: 0, ContentSHA256: hash}})
	page := rawEnvelope(t, catalog.TypeSnapshotPage, struct {
		catalog.Identity
		catalog.PageDigest
		Documents json.RawMessage `json:"documents"`
	}{id, catalog.PageDigest{PageIndex: 0, ContentSHA256: hash}, docs})
	end := rawEnvelope(t, catalog.TypeSnapshotEnd, struct {
		catalog.Identity
		catalog.Manifest
	}{id, catalog.Manifest{PageCount: 1, DocumentCount: 1, MembershipSHA256: membership, PagesSHA256: content}})
	page.SchemaVersion, end.SchemaVersion = 2, 2
	return []event.Envelope{page, end}
}

func TestCatalogConcurrentWorkersAndStagingLossReplay(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	epoch := uuid.New()
	if err := env.store.SetCatalogEpoch(ctx, "local", epoch, nil); err != nil {
		t.Fatal(err)
	}
	a, b := video("older observation"), video("newer observation")
	older, newer := catalogFullEvents(t, epoch, 1, a), catalogFullEvents(t, epoch, 2, b)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, batch := range [][]event.Envelope{older, newer, newer} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := env.events.IngestCatalog(ctx, batch); err != nil {
				t.Errorf("concurrent ingestion failed: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	var exact bool
	if err := env.store.Pool.QueryRow(ctx, `SELECT completed_generation=2
		AND (SELECT count(*)=1 FROM search.documents WHERE kind='local' AND eligible)
		AND (SELECT eligible FROM search.documents WHERE video_id=$1)
		FROM search.catalog_scopes`, b.ID).Scan(&exact); err != nil || !exact {
		t.Fatalf("overlapping workers lost newest exact membership: %v/%v", exact, err)
	}
	third := catalogFullEvents(t, epoch, 3, a)
	if result, err := env.events.IngestCatalog(ctx, third[:1]); err != nil || result.Accepted != 1 {
		t.Fatalf("stage before simulated loss: %+v/%v", result, err)
	}
	// Fault injection models a consistent older receiver checkpoint with the
	// same completed FULL but no pending-page receipt. This is not pg_restore proof.
	tx, err := env.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `DELETE FROM search.catalog_snapshots;
		UPDATE search.catalog_scopes SET highest_observed_generation=completed_generation`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if result, err := env.events.IngestCatalog(ctx, third); err != nil || result.Accepted != 2 || len(result.Failed) != 0 {
		t.Fatalf("frozen full replay after staging loss: %+v/%v", result, err)
	}
	// A wrong membership end fails after its immutable header write. Its
	// savepoint must remove that header so the correct end can still complete.
	fourth := catalogFullEvents(t, epoch, 4, b)
	var bad map[string]any
	if err := json.Unmarshal(fourth[1].Payload, &bad); err != nil {
		t.Fatal(err)
	}
	bad["membership_sha256"], _ = catalog.MembershipDigest([]uuid.UUID{uuid.New()})
	wrong := fourth[1]
	wrong.Payload, _ = json.Marshal(bad)
	result, err := env.events.IngestCatalog(ctx, []event.Envelope{fourth[0], wrong, fourth[1]})
	if err != nil || result.Accepted != 2 || len(result.Failed) != 1 {
		t.Fatalf("bad end prevented correct retry: %+v/%v", result, err)
	}
	if err := env.store.Pool.QueryRow(ctx, `SELECT completed_generation=4
		AND (SELECT count(*)=1 FROM search.documents WHERE eligible)
		AND (SELECT eligible FROM search.documents WHERE video_id=$1)
		FROM search.catalog_scopes`, b.ID).Scan(&exact); err != nil || !exact {
		t.Fatalf("recovery/retry final authority incorrect: %v/%v", exact, err)
	}
}
