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

func TestLockedCatalogEvidenceRequiresCompleteCurrentUnalteredPages(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	epoch := uuid.New()
	if err := env.store.SetCatalogEpoch(ctx, "local", epoch, nil); err != nil {
		t.Fatal(err)
	}
	id := catalog.Identity{Scope: "local", Epoch: epoch, Generation: 1}
	a, b := video("first proof"), video("second proof")
	first, _ := json.Marshal([]map[string]any{{"video": a, "eligible": true, "suppressed_reason": nil}})
	second, _ := json.Marshal([]map[string]any{{"video": b, "eligible": true, "suppressed_reason": nil}})
	_, hashA, _ := catalog.DocumentsDigest(first)
	_, hashB, _ := catalog.DocumentsDigest(second)
	membership, _ := catalog.MembershipDigest([]uuid.UUID{a.ID, b.ID})
	pages, _ := catalog.PagesDigest([]catalog.PageDigest{{PageIndex: 0, ContentSHA256: hashA}, {PageIndex: 1, ContentSHA256: hashB}})
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
	load := func(identity catalog.Identity, wantComplete bool) error {
		return inTx(func(tx pgx.Tx) error {
			got, err := store.LoadVerifiedCatalogSnapshot(ctx, tx, identity)
			if err == nil && (got != nil) != wantComplete {
				t.Fatalf("loaded complete=%v, want %v", got != nil, wantComplete)
			}
			return err
		})
	}
	if err := inTx(func(tx pgx.Tx) error {
		return store.StageCatalogPage(ctx, tx, id, catalog.PageDigest{PageIndex: 0, ContentSHA256: hashA}, first)
	}); err != nil {
		t.Fatal(err)
	}
	if err := load(id, false); err != nil {
		t.Fatal(err)
	}
	if err := inTx(func(tx pgx.Tx) error {
		return store.StageCatalogEnd(ctx, tx, id, catalog.Manifest{PageCount: 2, DocumentCount: 2, MembershipSHA256: membership, PagesSHA256: pages})
	}); err != nil {
		t.Fatal(err)
	}
	if err := load(id, false); err != nil {
		t.Fatal(err)
	}
	if err := inTx(func(tx pgx.Tx) error {
		return store.StageCatalogPage(ctx, tx, id, catalog.PageDigest{PageIndex: 1, ContentSHA256: hashB}, second)
	}); err != nil {
		t.Fatal(err)
	}
	if err := load(id, true); err != nil {
		t.Fatal(err)
	}
	wrongEpoch := id
	wrongEpoch.Epoch = uuid.New()
	if err := load(wrongEpoch, false); !errors.Is(err, store.ErrCatalogConflict) {
		t.Fatalf("loaded foreign epoch: %v", err)
	}
	if _, err := env.store.Pool.Exec(ctx, `UPDATE search.catalog_snapshots SET canonical_bytes=canonical_bytes+1`); err != nil {
		t.Fatal(err)
	}
	if err := load(id, false); !errors.Is(err, store.ErrCatalogConflict) {
		t.Fatalf("false persisted byte ledger accepted: %v", err)
	}
	if _, err := env.store.Pool.Exec(ctx, `UPDATE search.catalog_snapshots
		SET canonical_bytes=canonical_bytes-1, created_at=clock_timestamp()-interval '25 hours'`); err != nil {
		t.Fatal(err)
	}
	if err := load(id, false); !errors.Is(err, store.ErrCatalogConflict) {
		t.Fatalf("expired complete evidence accepted: %v", err)
	}
	var completed, live int64
	if err := env.store.Pool.QueryRow(ctx, `SELECT completed_generation,
		(SELECT count(*) FROM search.documents) FROM search.catalog_scopes`).Scan(&completed, &live); err != nil || completed != 0 || live != 0 {
		t.Fatalf("evidence loader changed authority: %d/%d, %v", completed, live, err)
	}
}
