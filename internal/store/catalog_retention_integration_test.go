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

func TestCatalogRetentionRemovesOnlyExpiredPendingPayloads(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	epoch := uuid.New()
	if err := env.store.SetCatalogEpoch(ctx, "local", epoch, nil); err != nil {
		t.Fatal(err)
	}
	inTx := func(f func(pgx.Tx) error) error {
		tx, err := env.store.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if err := f(tx); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	id := catalog.Identity{Scope: "local", Epoch: epoch, Generation: 1}
	empty, _ := catalog.MembershipDigest(nil)
	if err := inTx(func(tx pgx.Tx) error {
		if err := store.StageCatalogEnd(ctx, tx, id, catalog.Manifest{MembershipSHA256: empty, PagesSHA256: empty}); err != nil {
			return err
		}
		_, err := store.CompleteCatalogSnapshot(ctx, tx, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	id.Generation = 2
	raw, _ := json.Marshal([]map[string]any{{"video": video("expired evidence"), "eligible": true, "suppressed_reason": nil}})
	_, hash, _ := catalog.DocumentsDigest(raw)
	stage := func(tx pgx.Tx) error {
		return store.StageCatalogPage(ctx, tx, id, catalog.PageDigest{PageIndex: 0, ContentSHA256: hash}, raw)
	}
	if err := inTx(stage); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.Pool.Exec(ctx, `UPDATE search.catalog_snapshots SET created_at=clock_timestamp()-interval '25 hours'`); err != nil {
		t.Fatal(err)
	}
	for _, want := range []int64{1, 0} {
		if count, err := env.store.PruneCatalogSnapshots(ctx); err != nil || count != want {
			t.Fatalf("pruned %d, want %d: %v", count, want, err)
		}
	}
	var intact bool
	if err := env.store.Pool.QueryRow(ctx, `SELECT highest_observed_generation=2 AND completed_generation=1
		AND completed_membership_sha256=$1 AND completed_pages_sha256=$1
		AND (SELECT count(*)=1 FROM search.catalog_epochs)
		AND NOT EXISTS(SELECT 1 FROM search.catalog_snapshot_pages)
		FROM search.catalog_scopes`, empty).Scan(&intact); err != nil || !intact {
		t.Fatalf("retention changed permanent authority: %v/%v", intact, err)
	}
	if err := inTx(stage); !errors.Is(err, store.ErrCatalogConflict) {
		t.Fatalf("expired generation recreated after payload cleanup: %v", err)
	}
	id.Generation = 3
	if err := inTx(stage); err != nil {
		t.Fatal(err)
	}
	if count, err := env.store.PruneCatalogSnapshots(ctx); err != nil || count != 0 {
		t.Fatalf("fresh pending was pruned: %d/%v", count, err)
	}
}
