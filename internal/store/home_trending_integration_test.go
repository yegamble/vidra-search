//go:build integration

package store_test

import (
	"context"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/vidra/vidra-search/internal/store/sqlcgen"
)

// HomeTrending computes the document's age in float8 (numeric power() over
// every eligible document was nearly all of its time). This holds its order to
// the numeric definition it replaced, over ages from minutes to years, with
// tied scores, and with the published_at fallback.
func TestIntegrationHomeTrendingOrderMatchesTheNumericDefinition(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	ages := []string{"3 minutes", "50 minutes", "5 hours", "5 hours", "26 hours", "9 days", "9 days", "40 days", "400 days", "3 years"}
	n := 0
	for i, age := range ages {
		for _, views := range []int64{0, 7, 7, 1000 * int64(i+1)} {
			n++
			// Every third document has no published_at and falls back to
			// source_updated_at, as the statement's COALESCE does.
			if _, err := env.store.Pool.Exec(ctx, `INSERT INTO search.documents
				(video_id, title, eligible, views, published_at, source_updated_at)
				VALUES ($1, 'trend', true, $2,
				        CASE WHEN $4::int % 3 = 0 THEN NULL ELSE date_trunc('second', now()) - $3::interval END,
				        date_trunc('second', now()) - $3::interval)`,
				uuid.New(), views, age, n); err != nil {
				t.Fatalf("insert document: %v", err)
			}
		}
	}

	// One transaction: both statements must read the same now().
	tx, err := env.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `SELECT d.video_id FROM search.documents d WHERE d.eligible
		ORDER BY ( d.views / power(EXTRACT(EPOCH FROM (now() - COALESCE(d.published_at, d.source_updated_at))) / 3600.0 + 2, 1.5) )::double precision DESC,
		         d.video_id`)
	if err != nil {
		t.Fatalf("numeric order: %v", err)
	}
	var want []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		want = append(want, id)
	}
	rows.Close()

	got, err := sqlcgen.New(tx).HomeTrending(ctx, sqlcgen.HomeTrendingParams{Lim: 1000})
	if err != nil {
		t.Fatalf("HomeTrending: %v", err)
	}
	gotIDs := make([]uuid.UUID, 0, len(got))
	for _, r := range got {
		gotIDs = append(gotIDs, r.VideoID)
	}
	if len(want) != n || !slices.Equal(gotIDs, want) {
		t.Fatalf("HomeTrending order differs from the numeric definition (%d documents)\n got %v\nwant %v", n, gotIDs, want)
	}
}
