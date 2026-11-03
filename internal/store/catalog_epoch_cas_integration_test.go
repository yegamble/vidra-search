//go:build integration

package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/vidra/vidra-search/internal/store"
)

func TestCatalogEpochCASRetainsHistoryAndRequiresOriginalPredecessor(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	set := func(epoch uuid.UUID, expected *uuid.UUID, wantConflict bool) {
		t.Helper()
		err := env.store.SetCatalogEpoch(ctx, "local", epoch, expected)
		if wantConflict && !errors.Is(err, store.ErrCatalogConflict) || !wantConflict && err != nil {
			t.Fatalf("epoch %s predecessor %v conflict=%v: %v", epoch, expected, wantConflict, err)
		}
	}
	a, b, c := uuid.New(), uuid.New(), uuid.New()
	set(a, nil, false)
	set(a, nil, false)
	set(a, &a, true)
	set(b, nil, true)
	doc := video("projection survives explicit epoch reset")
	ingest(t, env, upsertEnvelope(t, doc))
	const empty = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if _, err := env.store.Pool.Exec(ctx, `UPDATE search.catalog_scopes
		SET highest_observed_generation=11, completed_generation=7, completed_at=now(),
		completed_membership_sha256=$1, completed_pages_sha256=$1;
		INSERT INTO search.catalog_snapshots(scope, epoch, generation)
		SELECT scope, epoch, 11 FROM search.catalog_scopes`, empty); err != nil {
		t.Fatal(err)
	}
	set(b, &a, false)
	set(b, &a, false)
	set(b, nil, true)
	set(a, &b, true) // retired epochs cannot be reused
	set(c, &a, true)
	var epoch uuid.UUID
	var highest, completed, staged int64
	if err := env.store.Pool.QueryRow(ctx, `SELECT epoch, highest_observed_generation,
		completed_generation, (SELECT count(*) FROM search.catalog_snapshots)
		FROM search.catalog_scopes`).Scan(&epoch, &highest, &completed, &staged); err != nil {
		t.Fatal(err)
	}
	if epoch != b || highest != 0 || completed != 0 || staged != 0 {
		t.Fatalf("reset authority = %s/%d/%d/%d", epoch, highest, completed, staged)
	}
	if got, err := env.store.Queries().GetDocument(ctx, doc.ID); err != nil || !got.Eligible {
		t.Fatalf("reset changed live projection: %+v, %v", got, err)
	}
	results := make(chan error, 2)
	for range 2 {
		go func() { results <- env.store.SetCatalogEpoch(ctx, "local", uuid.New(), &b) }()
	}
	first, second := <-results, <-results
	if (first == nil) == (second == nil) || first != nil && !errors.Is(first, store.ErrCatalogConflict) || second != nil && !errors.Is(second, store.ErrCatalogConflict) {
		t.Fatalf("concurrent resets = %v / %v", first, second)
	}
	for _, invalid := range []struct {
		scope string
		epoch uuid.UUID
	}{
		{"remote", uuid.New()}, {"", uuid.New()}, {"local", uuid.Nil},
	} {
		if err := env.store.SetCatalogEpoch(ctx, invalid.scope, invalid.epoch, nil); err == nil {
			t.Errorf("accepted invalid initialization: %+v", invalid)
		}
	}
}
