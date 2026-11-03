//go:build integration

package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/vidra/vidra-search/internal/catalog"
)

func TestCatalogCanonicalByteLedgerIsBoundedPendingState(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	epoch := uuid.New()
	if err := env.store.SetCatalogEpoch(ctx, "local", epoch, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.Pool.Exec(ctx, `UPDATE search.catalog_scopes SET highest_observed_generation=1;
		INSERT INTO search.catalog_snapshots(scope, epoch, generation)
		SELECT scope, epoch, 1 FROM search.catalog_scopes`); err != nil {
		t.Fatal(err)
	}
	var charged int64
	if err := env.store.Pool.QueryRow(ctx, `SELECT canonical_bytes FROM search.catalog_snapshots`).Scan(&charged); err != nil || charged != 0 {
		t.Fatalf("initial canonical charge = %d, %v", charged, err)
	}
	for _, invalid := range []int64{-1, catalog.MaxSnapshotBytes + 1} {
		if _, err := env.store.Pool.Exec(ctx, `UPDATE search.catalog_snapshots SET canonical_bytes=$1`, invalid); err == nil {
			t.Errorf("accepted out-of-budget canonical bytes %d", invalid)
		}
	}
	if _, err := env.store.Pool.Exec(ctx, `UPDATE search.catalog_snapshots SET canonical_bytes=$1`, int64(catalog.MaxSnapshotBytes)); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.Pool.Exec(ctx, `DELETE FROM search.catalog_snapshots`); err != nil {
		t.Fatal(err)
	}
	var high, complete int64
	if err := env.store.Pool.QueryRow(ctx, `SELECT highest_observed_generation, completed_generation
		FROM search.catalog_scopes`).Scan(&high, &complete); err != nil || high != 1 || complete != 0 {
		t.Fatalf("deleting charged pending state changed authority: %d/%d, %v", high, complete, err)
	}
}
