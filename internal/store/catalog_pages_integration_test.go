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

func TestCatalogPageStagingIsImmutableAndChargesDistinctCanonicalArrays(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	remote := video("remote ownership stays live")
	remote.Kind = "remote"
	ingest(t, env, upsertEnvelope(t, remote))
	epoch := uuid.New()
	if err := env.store.SetCatalogEpoch(ctx, "local", epoch, nil); err != nil {
		t.Fatal(err)
	}
	page := func(id uuid.UUID) ([]byte, []byte, string) {
		t.Helper()
		v := video("pending only")
		v.ID = id
		raw, _ := json.Marshal([]map[string]any{{"video": v, "eligible": true, "suppressed_reason": nil}})
		canonical, digest, err := catalog.DocumentsDigest(raw)
		if err != nil {
			t.Fatal(err)
		}
		return raw, canonical, digest
	}
	put := func(generation catalog.Generation, index int32, documents []byte, digest string) error {
		tx, err := env.store.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		identity := catalog.Identity{Scope: "local", Epoch: epoch, Generation: generation}
		if err := store.StageCatalogPage(ctx, tx, identity, catalog.PageDigest{PageIndex: index, ContentSHA256: digest}, documents); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	a, canonicalA, hashA := page(uuid.New())
	b, canonicalB, hashB := page(uuid.New())
	c, canonicalC, hashC := page(uuid.New())
	foreign, _, foreignHash := page(remote.ID)
	for _, replay := range [][]byte{a, canonicalA, a} {
		if err := put(1, 0, replay, hashA); err != nil {
			t.Fatal(err)
		}
	}
	for _, attempt := range []struct {
		generation catalog.Generation
		index      int32
		raw        []byte
		hash       string
	}{{1, 0, b, hashB}, {1, 1, a, hashA}, {1, 3, a, hashB}, {2, 0, foreign, foreignHash}} {
		if err := put(attempt.generation, attempt.index, attempt.raw, attempt.hash); !errors.Is(err, store.ErrCatalogConflict) {
			t.Fatalf("accepted conflict: %v", err)
		}
	}
	if err := put(1, 1, b, hashB); err != nil {
		t.Fatal(err)
	}
	var charge, pages, docs int64
	if err := env.store.Pool.QueryRow(ctx, `SELECT canonical_bytes,
		(SELECT count(*) FROM search.catalog_snapshot_pages),
		(SELECT sum(document_count) FROM search.catalog_snapshot_pages)
		FROM search.catalog_snapshots`).Scan(&charge, &pages, &docs); err != nil || charge != int64(len(canonicalA)+len(canonicalB)) || pages != 2 || docs != 2 {
		t.Fatalf("distinct page accounting = %d/%d/%d, %v", charge, pages, docs, err)
	}
	// Precharge the boundary without allocating a 512 MiB test catalog.
	if _, err := env.store.Pool.Exec(ctx, `UPDATE search.catalog_snapshots SET canonical_bytes=$1`, int64(catalog.MaxSnapshotBytes-len(canonicalC))); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := put(1, 2, c, hashC); err != nil {
			t.Fatalf("exact byte boundary or replay rejected: %v", err)
		}
	}
	d, _, hashD := page(uuid.New())
	if err := put(1, 3, d, hashD); !errors.Is(err, store.ErrCatalogQuota) {
		t.Fatalf("byte overflow = %v", err)
	}
	// A storage-only precharged count fixture isolates document quota arithmetic;
	// the HTTP business validator independently rejects these null documents.
	if _, err := env.store.Pool.Exec(ctx, `UPDATE search.catalog_snapshots SET canonical_bytes=0;
		UPDATE search.catalog_snapshot_pages SET documents=(SELECT jsonb_agg(NULL::text)
		FROM generate_series(1,100000)), document_count=100000 WHERE page_index=0`); err != nil {
		t.Fatal(err)
	}
	if err := put(1, 3, d, hashD); !errors.Is(err, store.ErrCatalogQuota) {
		t.Fatalf("document overflow = %v", err)
	}
	if err := put(1, catalog.MaxPages, d, hashD); !errors.Is(err, store.ErrCatalogQuota) {
		t.Fatalf("page overflow = %v", err)
	}
	if row, err := env.store.Queries().GetDocument(ctx, remote.ID); err != nil || !row.Eligible || row.Kind != "remote" {
		t.Fatalf("staging changed remote projection: %+v, %v", row, err)
	}
	var completed, localRows int64
	if err := env.store.Pool.QueryRow(ctx, `SELECT completed_generation,
		(SELECT count(*) FROM search.documents WHERE kind='local') FROM search.catalog_scopes`).Scan(&completed, &localRows); err != nil || completed != 0 || localRows != 0 {
		t.Fatalf("staging changed live authority: %d/%d, %v", completed, localRows, err)
	}
}
