//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vidra/vidra-search/internal/catalog"
	"github.com/vidra/vidra-search/internal/event"
)

func TestCatalogIngestRevalidatesReplayAndRollsBackFailedAdmission(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	foreign := video("foreign")
	foreign.Kind = "remote"
	ingest(t, env, upsertEnvelope(t, foreign))
	epoch := uuid.New()
	if err := env.store.SetCatalogEpoch(ctx, "local", epoch, nil); err != nil {
		t.Fatal(err)
	}
	v := video("frozen full")
	page := func(g string, doc event.VideoDoc) event.Envelope {
		documents, _ := json.Marshal([]map[string]any{{"video": doc, "eligible": true, "suppressed_reason": nil}})
		_, hash, _ := catalog.DocumentsDigest(documents)
		payload, _ := json.Marshal(map[string]any{"scope": "local", "epoch": epoch, "generation": g,
			"page_index": 0, "content_sha256": hash, "documents": json.RawMessage(documents)})
		return event.Envelope{EventID: uuid.New(), Type: catalog.TypeSnapshotPage,
			SchemaVersion: 2, OccurredAt: time.Now(), Payload: payload}
	}
	first := page("1", v)
	var payload struct {
		ContentSHA256 string `json:"content_sha256"`
	}
	if err := json.Unmarshal(first.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	membership, _ := catalog.MembershipDigest([]uuid.UUID{v.ID})
	content, _ := catalog.PagesDigest([]catalog.PageDigest{{PageIndex: 0, ContentSHA256: payload.ContentSHA256}})
	endPayload, _ := json.Marshal(map[string]any{"scope": "local", "epoch": epoch, "generation": "1",
		"page_count": 1, "document_count": 1, "membership_sha256": membership, "pages_sha256": content})
	end := event.Envelope{EventID: uuid.New(), Type: catalog.TypeSnapshotEnd, SchemaVersion: 2,
		OccurredAt: time.Now(), Payload: endPayload}
	apply := func(wantAccepted, wantFailed int, events ...event.Envelope) {
		t.Helper()
		result, err := env.events.IngestCatalog(ctx, events)
		if err != nil || result.Accepted != wantAccepted || result.Duplicates != 0 || len(result.Failed) != wantFailed {
			t.Fatalf("catalog result: %+v, err=%v", result, err)
		}
	}
	apply(2, 0, first, first) // Domain replay, never an inbox receipt shortcut.
	changed := v
	changed.Title = "changed under same event ID"
	conflicting := page("1", changed)
	conflicting.EventID = first.EventID
	foreign.Kind = "local"
	badHigher := page("2", foreign) // Fails after admission has superseded generation 1.
	apply(0, 2, conflicting, badHigher)
	var intact bool
	if err := env.store.Pool.QueryRow(ctx, `SELECT highest_observed_generation=1 AND completed_generation=0
		AND (SELECT count(*)=1 FROM search.catalog_snapshot_pages WHERE generation=1)
		FROM search.catalog_scopes`).Scan(&intact); err != nil || !intact {
		t.Fatalf("failed admission destroyed prior pending evidence: %v/%v", intact, err)
	}
	// A legacy inbox receipt using this event ID must not bypass v2 application.
	if _, err := env.store.Pool.Exec(ctx, `INSERT INTO search.events_inbox(event_id,type) VALUES($1,'old.receipt')`, end.EventID); err != nil {
		t.Fatal(err)
	}
	apply(1, 0, end)
	apply(1, 1, first, end) // Pruned completed pages fail; exact end replay succeeds.
	unknown, wrongVersion, missingID := end, end, end
	unknown.Type, wrongVersion.SchemaVersion, missingID.EventID = "catalog.target", 1, uuid.Nil
	apply(0, 3, unknown, wrongVersion, missingID)
	if err := env.store.Pool.QueryRow(ctx, `SELECT completed_generation=1 AND completed_document_count=1
		AND NOT EXISTS(SELECT 1 FROM search.catalog_snapshots)
		AND (SELECT count(*)=2 FROM search.documents WHERE eligible)
		AND (SELECT count(*)=2 FROM search.events_inbox) FROM search.catalog_scopes`).Scan(&intact); err != nil || !intact {
		t.Fatalf("completion or inbox boundary incorrect: %v/%v", intact, err)
	}
	var nextEnd map[string]any
	if err := json.Unmarshal(end.Payload, &nextEnd); err != nil {
		t.Fatal(err)
	}
	nextEnd["generation"] = "2"
	end.Payload, _ = json.Marshal(nextEnd)
	end.EventID = uuid.New()
	apply(1, 0, end) // Reordered end cannot publish authority before its page.
	if err := env.store.Pool.QueryRow(ctx, `SELECT completed_generation=1 AND highest_observed_generation=2
		FROM search.catalog_scopes`).Scan(&intact); err != nil || !intact {
		t.Fatalf("early end became authority: %v/%v", intact, err)
	}
	apply(1, 0, page("2", v))
	if err := env.store.Pool.QueryRow(ctx, `SELECT completed_generation=2 FROM search.catalog_scopes`).Scan(&intact); err != nil || !intact {
		t.Fatalf("last reordered page did not complete: %v/%v", intact, err)
	}
}
