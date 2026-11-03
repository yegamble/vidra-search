//go:build integration

package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vidra/vidra-search/internal/catalog"
	"github.com/vidra/vidra-search/internal/store"
)

func TestCatalogGenerationAdmissionRetainsExpiryAndCompletedFences(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	epoch := uuid.New()
	admit := func(identity catalog.Identity) error {
		tx, err := env.store.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if err := store.AdmitCatalogGeneration(ctx, tx, identity); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	id := catalog.Identity{Scope: "local", Epoch: epoch, Generation: 1}
	if err := admit(id); !errors.Is(err, store.ErrCatalogConflict) {
		t.Fatalf("uninitialized admission = %v", err)
	}
	local, remote := video("local stays live"), video("remote stays live")
	remote.Kind = "remote"
	ingest(t, env, upsertEnvelope(t, local), upsertEnvelope(t, remote))
	if err := env.store.SetCatalogEpoch(ctx, "local", epoch, nil); err != nil {
		t.Fatal(err)
	}
	for _, generation := range []catalog.Generation{1, 1, 3} {
		id.Generation = generation
		if err := admit(id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := env.store.Pool.Exec(ctx, `DELETE FROM search.catalog_snapshots;
		TRUNCATE search.events_inbox`); err != nil {
		t.Fatal(err)
	}
	for _, generation := range []catalog.Generation{1, 2, 3} {
		id.Generation = generation
		if err := admit(id); !errors.Is(err, store.ErrCatalogConflict) {
			t.Fatalf("expired/superseded generation %d admitted: %v", generation, err)
		}
	}
	const empty = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if _, err := env.store.Pool.Exec(ctx, `UPDATE search.catalog_scopes SET completed_generation=2,
		completed_at=now(), completed_membership_sha256=$1, completed_pages_sha256=$1`, empty); err != nil {
		t.Fatal(err)
	}
	id.Generation = 2
	if err := admit(id); !errors.Is(err, store.ErrCatalogAlreadyCompleted) {
		t.Fatalf("completed page must require manifest/status verification: %v", err)
	}
	results := make(chan error, 2)
	for _, generation := range []catalog.Generation{5, 6} {
		observation := catalog.Identity{Scope: "local", Epoch: epoch, Generation: generation}
		go func() { results <- admit(observation) }()
	}
	for range 2 {
		if err := <-results; err != nil && !errors.Is(err, store.ErrCatalogConflict) {
			t.Fatal(err)
		}
	}
	var high, completed, pending int64
	if err := env.store.Pool.QueryRow(ctx, `SELECT highest_observed_generation, completed_generation,
		(SELECT generation FROM search.catalog_snapshots) FROM search.catalog_scopes`).Scan(&high, &completed, &pending); err != nil {
		t.Fatal(err)
	}
	if high != 6 || pending != 6 || completed != 2 {
		t.Fatalf("concurrent observations changed fences: %d/%d/%d", high, completed, pending)
	}
	for _, observation := range []catalog.Identity{
		{Scope: "remote", Epoch: epoch, Generation: 7}, {Scope: "local", Epoch: uuid.New(), Generation: 7},
		{Scope: "local", Epoch: epoch, Generation: 0},
	} {
		if err := admit(observation); !errors.Is(err, store.ErrCatalogConflict) {
			t.Fatalf("admitted invalid ownership/fence %+v: %v", observation, err)
		}
	}
	for _, doc := range []uuid.UUID{local.ID, remote.ID} {
		if row, err := env.store.Queries().GetDocument(ctx, doc); err != nil || !row.Eligible {
			t.Fatalf("pending observation changed live document: %+v, %v", row, err)
		}
	}
}
