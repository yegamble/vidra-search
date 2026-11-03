//go:build integration

package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/vidra/vidra-search/internal/catalog"
)

func TestCatalogSnapshotSchemaBoundsOwnershipAndCascadesPayloads(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	q := env.store.Pool
	t.Cleanup(func() {
		if _, err := q.Exec(ctx, `TRUNCATE search.catalog_snapshot_pages,
			search.catalog_snapshots, search.catalog_scopes, search.catalog_epochs`); err != nil {
			t.Errorf("catalog cleanup: %v", err)
		}
	})
	epoch := uuid.New()
	if _, err := q.Exec(ctx, `INSERT INTO search.catalog_epochs(scope, epoch) VALUES ('local', $1)`, epoch); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Exec(ctx, `INSERT INTO search.catalog_scopes(scope, epoch, highest_observed_generation)
		VALUES ('local', $1, 1)`, epoch); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Exec(ctx, `INSERT INTO search.catalog_snapshots(scope, epoch, generation)
		VALUES ('local', $1, 1)`, epoch); err != nil {
		t.Fatal(err)
	}
	id := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	canonical, digest, err := catalog.DocumentsDigest([]byte(`[{
		"video":{"id":"11111111-1111-4111-8111-111111111111","kind":"local"},
		"eligible":true,"suppressed_reason":null}]`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.Exec(ctx, `INSERT INTO search.catalog_snapshot_pages
		(scope, epoch, generation, page_index, documents, document_count, content_sha256)
		VALUES ('local', $1, 1, 0, $2, 1, $3)`, epoch, canonical, digest); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`UPDATE search.catalog_snapshot_pages SET page_index = -1`,
		`UPDATE search.catalog_snapshot_pages SET document_count = 2`,
		`UPDATE search.catalog_snapshot_pages SET documents = '[]'::jsonb`,
		`UPDATE search.catalog_snapshot_pages SET content_sha256 = 'invalid'`,
		`UPDATE search.catalog_snapshot_pages SET generation = 2`,
		`UPDATE search.catalog_snapshots SET page_count = 1`,
		`INSERT INTO search.catalog_snapshots(scope, epoch, generation)
		 SELECT scope, epoch, 2 FROM search.catalog_scopes`,
	} {
		if _, err := q.Exec(ctx, statement); err == nil {
			t.Errorf("accepted inconsistent staging: %s", statement)
		}
	}
	membership, err := catalog.MembershipDigest([]uuid.UUID{id})
	if err != nil {
		t.Fatal(err)
	}
	pages, err := catalog.PagesDigest([]catalog.PageDigest{{PageIndex: 0, ContentSHA256: digest}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.Exec(ctx, `UPDATE search.catalog_snapshots SET page_count = 1,
		document_count = 1, membership_sha256 = $1, pages_sha256 = $2`, membership, pages); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Exec(ctx, `DELETE FROM search.catalog_snapshots WHERE scope = 'local'`); err != nil {
		t.Fatal(err)
	}
	var payloads, highest, completed int64
	if err := q.QueryRow(ctx, `SELECT (SELECT count(*) FROM search.catalog_snapshot_pages),
		highest_observed_generation, completed_generation FROM search.catalog_scopes`).Scan(&payloads, &highest, &completed); err != nil {
		t.Fatal(err)
	}
	if payloads != 0 || highest != 1 || completed != 0 {
		t.Fatalf("payload cleanup changed authority: %d/%d/%d", payloads, highest, completed)
	}
}
