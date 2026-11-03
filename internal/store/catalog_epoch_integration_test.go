//go:build integration

package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

func TestCatalogEpochSchemaRetainsFencesWithoutInbox(t *testing.T) {
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
	const empty = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if _, err := q.Exec(ctx, `INSERT INTO search.catalog_scopes(scope, epoch,
		highest_observed_generation, completed_generation, completed_at,
		completed_membership_sha256, completed_pages_sha256)
		VALUES ('local', $1, 11, 7, now(), $2, $2)`, epoch, empty); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Exec(ctx, `TRUNCATE search.events_inbox`); err != nil {
		t.Fatal(err)
	}
	var highest, complete int64
	var digest string
	if err := q.QueryRow(ctx, `SELECT highest_observed_generation, completed_generation,
		completed_membership_sha256 FROM search.catalog_scopes WHERE scope='local'`).Scan(&highest, &complete, &digest); err != nil {
		t.Fatal(err)
	}
	if highest != 11 || complete != 7 || digest != empty {
		t.Fatalf("fences changed after inbox cleanup: %d/%d/%s", highest, complete, digest)
	}
	for _, statement := range []string{
		`UPDATE search.catalog_scopes SET completed_generation = 12`,
		`UPDATE search.catalog_scopes SET highest_observed_generation = 6`,
		`UPDATE search.catalog_scopes SET completed_document_count = -1`,
		`UPDATE search.catalog_scopes SET completed_at = NULL`,
		`UPDATE search.catalog_scopes SET completed_membership_sha256 = NULL`,
		`UPDATE search.catalog_scopes SET completed_pages_sha256 = 'invalid'`,
		`UPDATE search.catalog_scopes SET epoch = '00000000-0000-0000-0000-000000000000'`,
		`INSERT INTO search.catalog_epochs(scope, epoch) VALUES ('remote', gen_random_uuid())`,
	} {
		if _, err := q.Exec(ctx, statement); err == nil {
			t.Errorf("accepted inconsistent catalog state: %s", statement)
		}
	}
	next := uuid.New()
	if _, err := q.Exec(ctx, `INSERT INTO search.catalog_epochs(scope, epoch, predecessor)
		VALUES ('local', $1, $2)`, next, epoch); err != nil {
		t.Fatal(err)
	}
	var predecessor uuid.UUID
	if err := q.QueryRow(ctx, `SELECT predecessor FROM search.catalog_epochs
		WHERE scope='local' AND epoch=$1`, next).Scan(&predecessor); err != nil || predecessor != epoch {
		t.Fatalf("epoch predecessor = %s, %v", predecessor, err)
	}
}
