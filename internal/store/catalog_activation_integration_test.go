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
	"github.com/vidra/vidra-search/internal/event"
	"github.com/vidra/vidra-search/internal/store"
)

func TestCatalogActivationIsAtomicLocalExactAndPreservesExistingCounters(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	keep, orphan, hidden, remote := video("old title"), video("orphan"), video("private"), video("remote")
	hidden.Privacy, remote.Kind, keep.Views = "private", "remote", 42
	ingest(t, env, upsertEnvelope(t, keep), upsertEnvelope(t, orphan), upsertEnvelope(t, hidden), upsertEnvelope(t, remote))
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
	full := func(g catalog.Generation, videos []event.VideoDoc, activate bool) error {
		return inTx(func(tx pgx.Tx) error {
			id := catalog.Identity{Scope: "local", Epoch: epoch, Generation: g}
			var members []uuid.UUID
			var docs []map[string]any
			for _, v := range videos {
				members = append(members, v.ID)
				docs = append(docs, map[string]any{"video": v, "eligible": true, "suppressed_reason": nil})
			}
			membership, _ := catalog.MembershipDigest(members)
			var pages []catalog.PageDigest
			if len(docs) > 0 {
				raw, _ := json.Marshal(docs)
				_, hash, _ := catalog.DocumentsDigest(raw)
				pages = append(pages, catalog.PageDigest{PageIndex: 0, ContentSHA256: hash})
				if err := store.StageCatalogPage(ctx, tx, id, pages[0], raw); err != nil {
					return err
				}
			}
			content, _ := catalog.PagesDigest(pages)
			if err := store.StageCatalogEnd(ctx, tx, id, catalog.Manifest{PageCount: int32(len(pages)),
				DocumentCount: int64(len(members)), MembershipSHA256: membership, PagesSHA256: content}); err != nil {
				return err
			}
			if activate {
				ok, err := store.CompleteCatalogSnapshot(ctx, tx, id)
				if err == nil && !ok {
					t.Fatal("complete evidence did not activate")
				}
				return err
			}
			return nil
		})
	}
	keep.Title, keep.Views = "canonical title", 7
	added := video("new seed")
	added.Views = 7
	added.Tags, added.UpdatedAt = nil, nil // Normalize nullable tags and use created_at evidence.
	if err := full(1, []event.VideoDoc{keep, added}, true); err != nil {
		t.Fatal(err)
	}
	var correct bool
	if err := env.store.Pool.QueryRow(ctx, `SELECT
		(SELECT title='canonical title' AND views=42 AND eligible FROM search.documents WHERE video_id=$1)
		AND (SELECT views=7 AND eligible AND cardinality(tags)=0 FROM search.documents WHERE video_id=$2)
		AND (SELECT NOT eligible AND suppressed_reason='catalog_orphan' FROM search.documents WHERE video_id=$3)
		AND (SELECT NOT eligible AND suppressed_reason='private' FROM search.documents WHERE video_id=$4)
		AND (SELECT eligible AND kind='remote' FROM search.documents WHERE video_id=$5)
		AND (SELECT completed_generation=1 FROM search.catalog_scopes)
		AND NOT EXISTS(SELECT 1 FROM search.catalog_snapshots)`, keep.ID, added.ID, orphan.ID, hidden.ID, remote.ID).Scan(&correct); err != nil || !correct {
		t.Fatalf("incorrect activated projection: %v/%v", correct, err)
	}
	// Repeated frozen metadata must neither rewrite the row nor overwrite counters.
	// This injects the receiver boundary; core does not currently emit stats.
	ingest(t, env, rawEnvelope(t, event.TypeVideoStats, map[string]any{"video_id": keep.ID, "views": 57, "likes": 3}))
	var before, after string
	if err := env.store.Pool.QueryRow(ctx, `SELECT xmin::text FROM search.documents WHERE video_id=$1`, keep.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := full(2, []event.VideoDoc{keep, added}, true); err != nil {
		t.Fatal(err)
	}
	if err := env.store.Pool.QueryRow(ctx, `SELECT xmin::text FROM search.documents WHERE video_id=$1`, keep.ID).Scan(&after); err != nil || before != after {
		t.Fatalf("unchanged row rewritten: %s/%s %v", before, after, err)
	}
	if err := env.store.Pool.QueryRow(ctx, `SELECT views=57 AND likes=3 FROM search.documents WHERE video_id=$1`, keep.ID).Scan(&correct); err != nil || !correct {
		t.Fatalf("existing counters overwritten: %v/%v", correct, err)
	}
	// Ownership can change after staging. Recheck at activation before any write.
	collision := video("late ownership collision")
	if err := full(3, []event.VideoDoc{collision}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.Pool.Exec(ctx, `INSERT INTO search.documents(video_id,kind,title,source_updated_at,eligible)
		VALUES($1,'remote','foreign owner',now(),true)`, collision.ID); err != nil {
		t.Fatal(err)
	}
	if err := inTx(func(tx pgx.Tx) error {
		_, err := store.CompleteCatalogSnapshot(ctx, tx, catalog.Identity{Scope: "local", Epoch: epoch, Generation: 3})
		return err
	}); !errors.Is(err, store.ErrCatalogConflict) {
		t.Fatalf("late foreign ownership accepted: %v", err)
	}
	if err := env.store.Pool.QueryRow(ctx, `SELECT (SELECT completed_generation=2 FROM search.catalog_scopes)
		AND (SELECT eligible FROM search.documents WHERE video_id=$1)
		AND (SELECT title='foreign owner' AND kind='remote' FROM search.documents WHERE video_id=$2)`, keep.ID, collision.ID).Scan(&correct); err != nil || !correct {
		t.Fatalf("failed activation changed live state: %v/%v", correct, err)
	}
	if err := full(4, nil, true); err != nil {
		t.Fatal(err)
	}
	if err := env.store.Pool.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM search.documents WHERE kind='local' AND eligible)
		AND (SELECT count(*)=2 FROM search.documents WHERE kind='remote' AND eligible)
		AND (SELECT completed_generation=4 AND completed_document_count=0 FROM search.catalog_scopes)`).Scan(&correct); err != nil || !correct {
		t.Fatalf("empty full scope/completion incorrect: %v/%v", correct, err)
	}
}
