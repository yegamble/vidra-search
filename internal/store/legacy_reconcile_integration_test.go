//go:build integration

package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/vidra/vidra-search/internal/event"
)

func TestLegacyEndCannotSuppressInterleavedCatalogDocuments(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	seen, fresh := video("seen by both sweeps"), video("published after sweep began")
	a, b := uuid.New(), uuid.New()
	endA := rawEnvelope(t, event.TypeReconcileEnd, map[string]any{"run_id": a, "total": 1})
	endB := rawEnvelope(t, event.TypeReconcileEnd, map[string]any{"run_id": b, "total": 1})
	result := ingest(t, env,
		rawEnvelope(t, event.TypeReconcileBegin, map[string]any{"run_id": a}),
		rawEnvelope(t, event.TypeReconcilePage, map[string]any{"run_id": a, "videos": []event.VideoDoc{seen}}),
		rawEnvelope(t, event.TypeReconcileBegin, map[string]any{"run_id": b}),
		rawEnvelope(t, event.TypeReconcilePage, map[string]any{"run_id": b, "videos": []event.VideoDoc{seen}}),
		upsertEnvelope(t, fresh), endA, endB,
	)
	if len(result.Failed) != 0 {
		t.Fatalf("legacy delivery failed: %+v", result)
	}
	check := func() {
		t.Helper()
		for _, id := range []uuid.UUID{seen.ID, fresh.ID} {
			doc, err := env.store.Queries().GetDocument(ctx, id)
			if err != nil || !doc.Eligible || doc.SuppressedReason != nil {
				t.Fatalf("legacy end changed eligibility for %s: %+v, %v", id, doc, err)
			}
		}
	}
	check()
	result = ingest(t, env, endA, endB)
	if result.Duplicates != 2 {
		t.Fatalf("legacy end replay = %+v", result)
	}
	check()
	result = ingest(t, env,
		rawEnvelope(t, event.TypeReconcilePage, map[string]any{"run_id": a, "videos": "invalid page"}),
		rawEnvelope(t, event.TypeReconcileEnd, map[string]any{"run_id": a, "total": 500}),
	)
	if len(result.Failed) != 1 {
		t.Fatalf("malformed page was not isolated: %+v", result)
	}
	check()
}
