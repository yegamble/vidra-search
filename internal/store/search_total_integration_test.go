//go:build integration

// Integration coverage for the search total-hit contract (Response.Total /
// TotalIsLowerBound / HasMore). These run the REAL SearchSimpleCount and
// SearchAdvancedRecall SQL against live Postgres, which is the only place the
// claim "the count shares the page query's predicates" can actually be proved —
// the unit tests in internal/search can only prove the same parameters were
// passed. Self-skips when DATABASE_URL / REDIS_URL are unset.
package store_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/vidra/vidra-search/internal/event"
	"github.com/vidra/vidra-search/internal/search"
)

// mustTotal fails unless the response carries a computed total.
func mustTotal(t *testing.T, resp search.Response) int {
	t.Helper()
	if resp.Total == nil {
		t.Fatalf("total was omitted; skip_count was not requested")
	}
	return *resp.Total
}

// TestIntegrationSimpleTotalMatchesPageQueryUnderEveryFilter is the real-SQL
// drift proof: with a limit far larger than the corpus the page IS the complete
// result set, so `total == len(ids)` holds if and only if the COUNT applies the
// same predicates the page query does. It is asserted separately for every
// filter the query supports, because a predicate can drift in just one of them.
func TestIntegrationSimpleTotalMatchesPageQueryUnderEveryFilter(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	// A corpus designed so each filter selects a different subset.
	enGo := video("widget golang guide", func(v *event.VideoDoc) {
		v.Tags = []string{"programming", "go"}
		v.Category = ptr("education")
		v.Language = ptr("en")
	})
	esGo := video("widget golang spanish", func(v *event.VideoDoc) {
		v.Tags = []string{"programming"}
		v.Category = ptr("education")
		v.Language = ptr("es")
	})
	sensitive := video("widget golang uncensored", func(v *event.VideoDoc) {
		v.IsSensitive = true
		v.Tags = []string{"programming", "go"}
		v.Category = ptr("entertainment")
		v.Language = ptr("en")
	})
	// Ineligible: must be counted by neither the page query nor the count.
	private := video("widget golang private", func(v *event.VideoDoc) { v.Privacy = "private" })
	ingest(t, env,
		upsertEnvelope(t, enGo), upsertEnvelope(t, esGo),
		upsertEnvelope(t, sensitive), upsertEnvelope(t, private))

	cases := []struct {
		name string
		req  search.Request
		want int
	}{
		{"no filters", search.Request{Query: "widget golang"}, 3},
		{"hide_sensitive", search.Request{Query: "widget golang", HideSensitive: true}, 2},
		{"tag", search.Request{Query: "widget golang", Tag: "go"}, 2},
		{"category", search.Request{Query: "widget golang", Category: "education"}, 2},
		{"language", search.Request{Query: "widget golang", Language: "en"}, 2},
		{"every filter at once", search.Request{
			Query: "widget golang", HideSensitive: true,
			Tag: "go", Category: "education", Language: "en",
		}, 1},
		{"filter matching nothing", search.Request{Query: "widget golang", Category: "nonexistent"}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := tc.req
			req.Limit = 200 // >> corpus, so the page is the whole result set
			resp, err := env.search.Search(ctx, req)
			if err != nil {
				t.Fatalf("search: %v", err)
			}
			total := mustTotal(t, resp)
			if total != len(resp.IDs) {
				t.Errorf("total = %d but the unpaged page returned %d ids — the COUNT "+
					"predicates have drifted from the page query's", total, len(resp.IDs))
			}
			if total != tc.want {
				t.Errorf("total = %d, want %d", total, tc.want)
			}
			if resp.TotalIsLowerBound {
				t.Errorf("simple-mode totals are exact; total_is_lower_bound must be false")
			}
			if resp.HasMore {
				t.Errorf("the whole result set fit in one page; has_more must be false")
			}
		})
	}
}

// TestIntegrationSimplePagingWalksExactlyTotal proves the total is a usable
// stop condition against real SQL: paging on has_more visits every hit exactly
// once and stops precisely at total.
func TestIntegrationSimplePagingWalksExactlyTotal(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	const corpus = 25
	evs := make([]event.Envelope, 0, corpus)
	for i := 0; i < corpus; i++ {
		evs = append(evs, upsertEnvelope(t, video(fmt.Sprintf("gadget review number %d", i))))
	}
	ingest(t, env, evs...)

	seen := map[string]bool{}
	pages := 0
	wantTotal := -1
	for offset := 0; ; offset += 10 {
		resp, err := env.search.Search(ctx, search.Request{Query: "gadget review", Limit: 10, Offset: offset})
		if err != nil {
			t.Fatalf("offset %d: %v", offset, err)
		}
		total := mustTotal(t, resp)
		if wantTotal == -1 {
			wantTotal = total
		} else if total != wantTotal {
			t.Fatalf("offset %d: total changed mid-walk (%d then %d)", offset, wantTotal, total)
		}
		if len(resp.IDs) == 0 {
			t.Fatalf("offset %d: empty page while the previous page said has_more", offset)
		}
		for _, h := range resp.IDs {
			if seen[h.VideoID] {
				t.Errorf("offset %d: %s served twice", offset, h.VideoID)
			}
			seen[h.VideoID] = true
		}
		pages++
		if pages > 10 {
			t.Fatalf("paging did not terminate — has_more never went false")
		}
		if !resp.HasMore {
			break
		}
	}
	if wantTotal != corpus {
		t.Errorf("total = %d, want the %d-doc corpus", wantTotal, corpus)
	}
	if len(seen) != corpus {
		t.Errorf("walked %d distinct ids, want %d", len(seen), corpus)
	}
}

// TestIntegrationAdvancedRecallBoundary is the regression test for the paging
// cliff, run against real recall SQL: with a corpus larger than the base recall
// block, the page that used to come back empty must come back full, and the
// response must say plainly whether the total is exact or a lower bound.
func TestIntegrationAdvancedRecallBoundary(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	// Just past the 500-document base recall block.
	const corpus = 520
	for start := 0; start < corpus; start += 130 {
		evs := make([]event.Envelope, 0, 130)
		for i := start; i < start+130 && i < corpus; i++ {
			evs = append(evs, upsertEnvelope(t, video(fmt.Sprintf("sprocket clip %d", i))))
		}
		ingest(t, env, evs...)
	}
	if n := countRows(t, env, "SELECT count(*) FROM search.documents WHERE eligible"); n != corpus {
		t.Fatalf("seeded %d eligible documents, want %d", n, corpus)
	}

	// Last page inside the base block: full, and honest that it is not the end.
	last, err := env.search.Search(ctx, search.Request{
		Query: "sprocket clip", Mode: "advanced", Limit: 20, Offset: 480,
	})
	if err != nil {
		t.Fatalf("search offset 480: %v", err)
	}
	if len(last.IDs) != 20 {
		t.Errorf("offset 480: ids = %d, want 20", len(last.IDs))
	}
	if got := mustTotal(t, last); got != 500 {
		t.Errorf("offset 480: total = %d, want the 500-doc recall window", got)
	}
	if !last.TotalIsLowerBound {
		t.Errorf("offset 480: the corpus overflows the recall window; total_is_lower_bound must be true")
	}
	if !last.HasMore {
		t.Errorf("offset 480: has_more must be true — 20 more documents are reachable")
	}

	// The page that used to be permanently empty.
	next, err := env.search.Search(ctx, search.Request{
		Query: "sprocket clip", Mode: "advanced", Limit: 20, Offset: 500,
	})
	if err != nil {
		t.Fatalf("search offset 500: %v", err)
	}
	if len(next.IDs) == 0 {
		t.Fatalf("offset 500 returned an EMPTY page — the recall cliff is back")
	}
	if got := mustTotal(t, next); got != corpus {
		t.Errorf("offset 500: total = %d, want the exact %d-doc corpus (the window widened past it)", got, corpus)
	}
	if next.TotalIsLowerBound {
		t.Errorf("offset 500: the widened window covers the whole corpus; total is exact")
	}
	if next.HasMore {
		t.Errorf("offset 500: that is the last page; has_more must be false")
	}
}

// TestIntegrationSkipCountOmitsTotal proves skip_count reaches the SQL layer:
// the total is absent while the page and has_more are unaffected.
func TestIntegrationSkipCountOmitsTotal(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	evs := make([]event.Envelope, 0, 5)
	for i := 0; i < 5; i++ {
		evs = append(evs, upsertEnvelope(t, video(fmt.Sprintf("doohickey demo %d", i))))
	}
	ingest(t, env, evs...)

	counted, err := env.search.Search(ctx, search.Request{Query: "doohickey demo", Limit: 2})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if got := mustTotal(t, counted); got != 5 {
		t.Fatalf("total = %d, want 5", got)
	}

	skipped, err := env.search.Search(ctx, search.Request{Query: "doohickey demo", Limit: 2, SkipCount: true})
	if err != nil {
		t.Fatalf("search skip_count: %v", err)
	}
	if skipped.Total != nil {
		t.Errorf("total = %d, want it omitted under skip_count", *skipped.Total)
	}
	if len(skipped.IDs) != len(counted.IDs) {
		t.Errorf("skip_count changed the page: %d ids vs %d", len(skipped.IDs), len(counted.IDs))
	}
	if !skipped.HasMore {
		t.Errorf("has_more must stay exact under skip_count")
	}
}
