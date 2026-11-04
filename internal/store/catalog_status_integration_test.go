//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"math"
	"strconv"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vidra/vidra-search/internal/catalog"
	"github.com/vidra/vidra-search/internal/store"
)

func TestCatalogStatusSeparatesPendingDiagnosticsFromCompletedAuthority(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	check := func(epoch *uuid.UUID, highest, completed string, pending int32) {
		t.Helper()
		status, err := env.store.ReadCatalogStatus(ctx, "local")
		if err != nil {
			t.Fatal(err)
		}
		if (status.Epoch == nil) != (epoch == nil) || epoch != nil && *status.Epoch != *epoch ||
			status.HighestObservedGeneration != highest || status.CompletedGeneration != completed ||
			status.Ready != (completed != "0") || status.StagedSnapshots != pending ||
			(status.CompletedManifest == nil) != (completed == "0") ||
			(status.CompletedAt == nil) != (completed == "0") || (status.OldestStagedAt == nil) != (pending == 0) {
			t.Fatalf("inconsistent status: %+v", status)
		}
		wire, err := json.Marshal(status)
		if err != nil {
			t.Fatal(err)
		}
		fields, err := catalog.ObjectFields(wire, []string{"protocol_version", "supported_scopes", "supported_event_types",
			"limits", "scope", "epoch", "ready", "highest_observed_generation", "completed_generation", "completed_at",
			"completed_manifest", "staged_snapshots", "oldest_staged_at"}, []string{"epoch", "completed_at", "completed_manifest", "oldest_staged_at"})
		if err != nil || string(fields["completed_generation"]) != strconv.Quote(completed) {
			t.Fatalf("missing explicit status shape/string counter: %s, %v", wire, err)
		}
		if status.ProtocolVersion != 2 || len(status.SupportedScopes) != 1 || status.SupportedScopes[0] != "local" ||
			len(status.SupportedEventTypes) != 2 || status.SupportedEventTypes[0] != catalog.TypeSnapshotPage || status.SupportedEventTypes[1] != catalog.TypeSnapshotEnd ||
			status.Limits.MaxRequestBytes != catalog.MaxRequestBytes || status.Limits.MaxSnapshotBytes != catalog.MaxSnapshotBytes ||
			status.Limits.MaxDocuments != catalog.MaxDocuments || status.Limits.MaxPages != catalog.MaxPages {
			t.Fatalf("incorrect capability/resource shape: %+v", status)
		}
	}
	check(nil, "0", "0", 0)
	epoch := uuid.New()
	if err := env.store.SetCatalogEpoch(ctx, "local", epoch, nil); err != nil {
		t.Fatal(err)
	}
	check(&epoch, "0", "0", 0)
	id := catalog.Identity{Scope: "local", Epoch: epoch, Generation: math.MaxInt64}
	empty, _ := catalog.MembershipDigest(nil)
	manifest := catalog.Manifest{MembershipSHA256: empty, PagesSHA256: empty}
	tx, err := env.store.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := store.StageCatalogEnd(ctx, tx, id, manifest); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	maxGeneration := strconv.FormatInt(math.MaxInt64, 10)
	check(&epoch, maxGeneration, "0", 1)
	tx, err = env.store.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if ok, err := store.CompleteCatalogSnapshot(ctx, tx, id); err != nil || !ok {
		t.Fatalf("empty full completion: %v/%v", ok, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	check(&epoch, maxGeneration, maxGeneration, 0)
	status, err := env.store.ReadCatalogStatus(ctx, "local")
	if err != nil || *status.CompletedManifest != manifest {
		t.Fatalf("completed manifest not bound: %+v/%v", status, err)
	}
	next := uuid.New()
	if err := env.store.SetCatalogEpoch(ctx, "local", next, &epoch); err != nil {
		t.Fatal(err)
	}
	check(&next, "0", "0", 0)
	if _, err := env.store.ReadCatalogStatus(ctx, "remote"); err == nil {
		t.Fatal("unsupported status scope accepted")
	}
}
