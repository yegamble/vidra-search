//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vidra/vidra-search/internal/catalog"
	"github.com/vidra/vidra-search/internal/store"
)

func TestCatalogEndStagingIsImmutableAndCompletionRequiresExactManifest(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	epoch := uuid.New()
	if err := env.store.SetCatalogEpoch(ctx, "local", epoch, nil); err != nil {
		t.Fatal(err)
	}
	id := catalog.Identity{Scope: "local", Epoch: epoch, Generation: 1}
	v := video("manifest receipt only")
	raw, _ := json.Marshal([]map[string]any{{"video": v, "eligible": true, "suppressed_reason": nil}})
	_, content, _ := catalog.DocumentsDigest(raw)
	membership, _ := catalog.MembershipDigest([]uuid.UUID{v.ID})
	pages, _ := catalog.PagesDigest([]catalog.PageDigest{{PageIndex: 0, ContentSHA256: content}})
	manifest := catalog.Manifest{PageCount: 1, DocumentCount: 1, MembershipSHA256: membership, PagesSHA256: pages}
	inTx := func(apply func(pgx.Tx) error) error {
		tx, err := env.store.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if err := apply(tx); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	end := func(m catalog.Manifest) error {
		return inTx(func(tx pgx.Tx) error { return store.StageCatalogEnd(ctx, tx, id, m) })
	}
	for range 2 {
		if err := end(manifest); err != nil { // End may arrive before any page.
			t.Fatal(err)
		}
	}
	different := manifest
	different.DocumentCount = 2
	if err := end(different); !errors.Is(err, store.ErrCatalogConflict) {
		t.Fatalf("changed immutable manifest accepted: %v", err)
	}
	if err := inTx(func(tx pgx.Tx) error {
		return store.StageCatalogPage(ctx, tx, id, catalog.PageDigest{PageIndex: 1, ContentSHA256: content}, raw)
	}); !errors.Is(err, store.ErrCatalogConflict) {
		t.Fatalf("out-of-manifest page accepted: %v", err)
	}
	if err := inTx(func(tx pgx.Tx) error {
		return store.StageCatalogPage(ctx, tx, id, catalog.PageDigest{PageIndex: 0, ContentSHA256: content}, raw)
	}); err != nil {
		t.Fatal(err)
	}
	if err := end(manifest); err != nil {
		t.Fatal(err)
	}
	var completed, live int64
	if err := env.store.Pool.QueryRow(ctx, `SELECT completed_generation,
		(SELECT count(*) FROM search.documents) FROM search.catalog_scopes`).Scan(&completed, &live); err != nil || completed != 0 || live != 0 {
		t.Fatalf("end receipt became authority: %d/%d, %v", completed, live, err)
	}
	// Isolate replay of a completed checkpoint; activation is a separate operation.
	if _, err := env.store.Pool.Exec(ctx, `UPDATE search.catalog_scopes
		SET completed_generation=1, completed_at=now(), completed_page_count=1,
		completed_document_count=1, completed_membership_sha256=$1, completed_pages_sha256=$2`, membership, pages); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.Pool.Exec(ctx, `DELETE FROM search.catalog_snapshots`); err != nil {
		t.Fatal(err)
	}
	if err := end(manifest); err != nil {
		t.Fatalf("completed manifest retry rejected: %v", err)
	}
	if err := end(different); !errors.Is(err, store.ErrCatalogConflict) {
		t.Fatalf("different completed manifest accepted: %v", err)
	}
	id.Generation = 2
	if err := inTx(func(tx pgx.Tx) error {
		return store.StageCatalogPage(ctx, tx, id, catalog.PageDigest{PageIndex: 1, ContentSHA256: content}, raw)
	}); err != nil {
		t.Fatal(err)
	}
	if err := end(manifest); !errors.Is(err, store.ErrCatalogConflict) {
		t.Fatalf("end accepted an existing page outside its range: %v", err)
	}
	id.Generation = 1
	if err := end(manifest); err != nil {
		t.Fatal(err)
	}
	var highest, pending int64
	if err := env.store.Pool.QueryRow(ctx, `SELECT highest_observed_generation,
		(SELECT generation FROM search.catalog_snapshots) FROM search.catalog_scopes`).Scan(&highest, &pending); err != nil || highest != 2 || pending != 2 {
		t.Fatalf("completed replay changed newer pending state: %d/%d, %v", highest, pending, err)
	}
}
