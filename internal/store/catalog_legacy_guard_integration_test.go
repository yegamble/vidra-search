//go:build integration

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vidra/vidra-search/internal/event"
	"github.com/vidra/vidra-search/internal/store"
)

func TestCatalogEpochRejectsLegacyWritesBeforeInboxButAllowsIndependentEvents(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	doc := video("canonical title")
	initial := upsertEnvelope(t, doc)
	ingest(t, env, initial)
	if err := env.store.SetCatalogEpoch(ctx, "local", uuid.New(), nil); err != nil {
		t.Fatal(err)
	}
	legacy := []event.Envelope{initial} // Old successful receipts cannot bypass admission.
	for _, typ := range []string{event.TypeVideoUpsert, event.TypeVideoSuppress,
		event.TypeChannelUpsert, event.TypeChannelDelete, event.TypeUserSuppress,
		event.TypeReconcileBegin, event.TypeReconcilePage, event.TypeReconcileEnd} {
		legacy = append(legacy, rawEnvelope(t, typ, map[string]any{}))
	}
	for range 2 {
		got := ingest(t, env, legacy...)
		if got.Accepted != 0 || got.Duplicates != 0 || len(got.Failed) != len(legacy) {
			t.Fatalf("legacy writes accepted or tombstoned: %+v", got)
		}
	}
	got := ingest(t, env,
		rawEnvelope(t, event.TypeVideoStats, map[string]any{"video_id": doc.ID, "views": 42, "likes": 3}),
		rawEnvelope(t, event.TypeConfigUpdated, map[string]any{"settings": map[string]any{"search_mode": "advanced"}}),
		rawEnvelope(t, event.TypeUserHistoryDel, map[string]any{"user_id": uuid.New(), "scope": "all"}),
	)
	if len(got.Failed) != 0 || got.Accepted != 3 {
		t.Fatalf("independent events rejected: %+v", got)
	}
	row, err := env.store.Queries().GetDocument(ctx, doc.ID)
	if err != nil || !row.Eligible || row.Title != doc.Title || row.Views != 42 {
		t.Fatalf("legacy boundary changed projection: %+v, %v", row, err)
	}
}

func TestCatalogInitializationSerializesAgainstLegacyAdmission(t *testing.T) {
	env := newTestEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tx, err := env.store.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := store.LockCatalog(ctx, tx); err != nil {
		t.Fatal(err)
	}
	waiters := func(want int) {
		t.Helper()
		for {
			var n int
			if err := env.store.Pool.QueryRow(ctx, `SELECT count(*) FROM pg_locks
				WHERE locktype='advisory' AND NOT granted
				AND database=(SELECT oid FROM pg_database WHERE datname=current_database())`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n >= want {
				return
			}
			select {
			case <-ctx.Done():
				t.Fatalf("catalog writers did not wait on shared lock: %v", ctx.Err())
			case <-time.After(5 * time.Millisecond):
			}
		}
	}
	initialized := make(chan error, 1)
	go func() { initialized <- env.store.SetCatalogEpoch(ctx, "local", uuid.New(), nil) }()
	waiters(1)
	result := make(chan event.Result, 1)
	failure := make(chan error, 1)
	ev := upsertEnvelope(t, video("must not land after bootstrap"))
	go func() {
		got, err := env.events.Ingest(ctx, []event.Envelope{ev})
		result <- got
		failure <- err
	}()
	waiters(2)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-initialized; err != nil {
		t.Fatal(err)
	}
	got := <-result
	if err := <-failure; err != nil || got.Accepted != 0 || len(got.Failed) != 1 {
		t.Fatalf("legacy admission crossed bootstrap: %+v, %v", got, err)
	}
}
