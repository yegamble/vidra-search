package search

import (
	"context"
	"encoding/binary"
	"errors"
	"strconv"
	"testing"

	"github.com/google/uuid"

	"github.com/vidra/vidra-search/internal/ranking"
	"github.com/vidra/vidra-search/internal/store/sqlcgen"
)

// These tests cover the paging contract on Response — the total, its
// lower-bound qualifier, and has_more — against a fake Querier, so they run in
// the default `make ci` gate with no Postgres. The half that needs REAL SQL
// (does the count actually agree with the page query?) lives in
// internal/store/search_total_integration_test.go.

// --- fakes ---------------------------------------------------------------

// testID returns a deterministic uuid for corpus position i.
func testID(i int) uuid.UUID {
	var u uuid.UUID
	binary.BigEndian.PutUint64(u[8:], uint64(i))
	return u
}

// fakeQuerier serves a synthetic corpus of `corpus` matching documents and
// records every params struct it is handed, so a test can assert exactly what
// the service asked the database for.
type fakeQuerier struct {
	corpus int // documents that match the query + filters

	simpleCalls []sqlcgen.SearchSimpleParams
	countCalls  []sqlcgen.SearchSimpleCountParams
	recallCalls []sqlcgen.SearchAdvancedRecallParams

	countErr error
}

func (f *fakeQuerier) SearchSimple(_ context.Context, arg sqlcgen.SearchSimpleParams) ([]sqlcgen.SearchSimpleRow, error) {
	f.simpleCalls = append(f.simpleCalls, arg)
	rows := []sqlcgen.SearchSimpleRow{}
	for i := int(arg.Off); i < f.corpus && len(rows) < int(arg.Lim); i++ {
		rows = append(rows, sqlcgen.SearchSimpleRow{VideoID: testID(i), Score: float64(f.corpus - i)})
	}
	return rows, nil
}

func (f *fakeQuerier) SearchSimpleCount(_ context.Context, arg sqlcgen.SearchSimpleCountParams) (int64, error) {
	f.countCalls = append(f.countCalls, arg)
	if f.countErr != nil {
		return 0, f.countErr
	}
	return int64(f.corpus), nil
}

func (f *fakeQuerier) SearchAdvancedRecall(_ context.Context, arg sqlcgen.SearchAdvancedRecallParams) ([]sqlcgen.SearchAdvancedRecallRow, error) {
	f.recallCalls = append(f.recallCalls, arg)
	rows := []sqlcgen.SearchAdvancedRecallRow{}
	for i := 0; i < f.corpus && len(rows) < int(arg.Lim); i++ {
		rows = append(rows, sqlcgen.SearchAdvancedRecallRow{
			VideoID: testID(i),
			TsRank:  float64(f.corpus - i),
		})
	}
	return rows, nil
}

func (f *fakeQuerier) NeighborAffinity(context.Context, sqlcgen.NeighborAffinityParams) ([]sqlcgen.NeighborAffinityRow, error) {
	return nil, nil
}

func (f *fakeQuerier) UserChannelAffinity(context.Context, uuid.UUID) ([]sqlcgen.UserChannelAffinityRow, error) {
	return nil, nil
}

func (f *fakeQuerier) NeighborScoresFromSeeds(context.Context, sqlcgen.NeighborScoresFromSeedsParams) ([]sqlcgen.NeighborScoresFromSeedsRow, error) {
	return nil, nil
}

// orderPreservingRanker keeps the recall order so the paging assertions test the
// windowing, not the ranker's feature blend.
type orderPreservingRanker struct{}

func (orderPreservingRanker) Rerank(docs []ranking.Doc) []ranking.Ranked {
	out := make([]ranking.Ranked, 0, len(docs))
	for i, d := range docs {
		out = append(out, ranking.Ranked{VideoID: d.VideoID, Score: float64(len(docs) - i)})
	}
	return out
}
func (orderPreservingRanker) Version() string { return "test-order-preserving" }

type stubRankerProvider struct{}

func (stubRankerProvider) RankerFor(string) (ranking.Ranker, string) {
	r := orderPreservingRanker{}
	return r, r.Version()
}

func newTestService(f *fakeQuerier) *Service {
	return NewService(f, stubRankerProvider{}, nil, nil, nil)
}

// --- simple mode ---------------------------------------------------------

// TestSimpleTotalIsExact proves simple mode reports the full match count, not the
// page size, and that the count is not inferred from the page.
func TestSimpleTotalIsExact(t *testing.T) {
	f := &fakeQuerier{corpus: 137}
	resp, err := newTestService(f).Search(context.Background(), Request{Query: "golang", Limit: 20})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if resp.Total == nil {
		t.Fatalf("total must be computed by default (skip_count defaults to false)")
	}
	if *resp.Total != 137 {
		t.Errorf("total = %d, want 137", *resp.Total)
	}
	if resp.TotalIsLowerBound {
		t.Errorf("simple-mode total is exact; total_is_lower_bound must be false")
	}
	if len(resp.IDs) != 20 {
		t.Errorf("ids = %d, want the requested page of 20", len(resp.IDs))
	}
	if !resp.HasMore {
		t.Errorf("has_more must be true with 117 hits left after the page")
	}
}

// TestSimpleCountSharesPageQueryPredicates is the drift guard at the call site:
// under EVERY filter combination, the params handed to the count query must be
// the same values handed to the page query. (That the two SQL bodies then apply
// them identically is asserted by TestSearchCountSharesSimpleWhereClause in
// internal/store.)
func TestSimpleCountSharesPageQueryPredicates(t *testing.T) {
	reqs := []struct {
		name string
		req  Request
	}{
		{"no filters", Request{Query: "golang"}},
		{"hide_sensitive", Request{Query: "golang", HideSensitive: true}},
		{"tag", Request{Query: "golang", Tag: "go"}},
		{"category", Request{Query: "golang", Category: "tech"}},
		{"language", Request{Query: "golang", Language: "es"}},
		{"every filter at once", Request{
			Query: "golang", HideSensitive: true, Tag: "go", Category: "tech", Language: "es",
			Limit: 5, Offset: 40,
		}},
	}
	for _, tc := range reqs {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeQuerier{corpus: 200}
			if _, err := newTestService(f).Search(context.Background(), tc.req); err != nil {
				t.Fatalf("search: %v", err)
			}
			if len(f.simpleCalls) != 1 || len(f.countCalls) != 1 {
				t.Fatalf("want exactly one page call and one count call, got %d/%d", len(f.simpleCalls), len(f.countCalls))
			}
			page, count := f.simpleCalls[0], f.countCalls[0]
			if page.Query != count.Query {
				t.Errorf("query: page %q vs count %q", page.Query, count.Query)
			}
			if page.HideSensitive != count.HideSensitive {
				t.Errorf("hide_sensitive: page %v vs count %v", page.HideSensitive, count.HideSensitive)
			}
			if !sameOpt(page.Tag, count.Tag) {
				t.Errorf("tag: page %v vs count %v", optStrVal(page.Tag), optStrVal(count.Tag))
			}
			if !sameOpt(page.Category, count.Category) {
				t.Errorf("category: page %v vs count %v", optStrVal(page.Category), optStrVal(count.Category))
			}
			if !sameOpt(page.Language, count.Language) {
				t.Errorf("language: page %v vs count %v", optStrVal(page.Language), optStrVal(count.Language))
			}
		})
	}
}

func sameOpt(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func optStrVal(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

// TestSimpleHasMoreIsExactAtTheLastPage proves has_more comes from the
// over-fetched sentinel row and not from `len(ids) == limit`, which would report
// "more" on an exactly-full final page.
func TestSimpleHasMoreIsExactAtTheLastPage(t *testing.T) {
	// 40 hits, page 2 of 2 is exactly full: a naive len(ids)==limit test says
	// "more", the sentinel row says "done".
	f := &fakeQuerier{corpus: 40}
	resp, err := newTestService(f).Search(context.Background(), Request{Query: "golang", Limit: 20, Offset: 20})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(resp.IDs) != 20 {
		t.Fatalf("ids = %d, want a full final page of 20", len(resp.IDs))
	}
	if resp.HasMore {
		t.Errorf("has_more must be false on an exactly-full final page")
	}
	if got := f.simpleCalls[0].Lim; got != 21 {
		t.Errorf("page query LIMIT = %d, want limit+1 (the has_more sentinel)", got)
	}
	if resp.Total == nil || *resp.Total != 40 {
		t.Errorf("total = %s, want 40", totalOf(resp))
	}
}

// TestSimpleSkipCountOmitsTotalButKeepsHasMore proves skip_count elides the
// COUNT round-trip entirely while has_more stays exact.
func TestSimpleSkipCountOmitsTotalButKeepsHasMore(t *testing.T) {
	f := &fakeQuerier{corpus: 100}
	resp, err := newTestService(f).Search(context.Background(), Request{Query: "golang", Limit: 20, SkipCount: true})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(f.countCalls) != 0 {
		t.Errorf("skip_count must not issue the COUNT query, got %d calls", len(f.countCalls))
	}
	if resp.Total != nil {
		t.Errorf("total = %d, want it omitted entirely under skip_count", *resp.Total)
	}
	if !resp.HasMore {
		t.Errorf("has_more must stay exact under skip_count")
	}
}

// TestSimpleCountErrorSurfaces proves a failing count is an error, not a
// silently-omitted total (which the client would read as skip_count).
func TestSimpleCountErrorSurfaces(t *testing.T) {
	f := &fakeQuerier{corpus: 10, countErr: errors.New("boom")}
	if _, err := newTestService(f).Search(context.Background(), Request{Query: "golang"}); err == nil {
		t.Fatalf("want the count error surfaced, got nil")
	}
}

// TestEmptyQueryReportsExactZero proves the short-circuit path still carries a
// total, so "no results" is never confused with "total not computed".
func TestEmptyQueryReportsExactZero(t *testing.T) {
	f := &fakeQuerier{corpus: 100}
	resp, err := newTestService(f).Search(context.Background(), Request{Query: "   "})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if resp.Total == nil || *resp.Total != 0 {
		t.Errorf("total = %s, want an exact 0", totalOf(resp))
	}
	if resp.TotalIsLowerBound || resp.HasMore {
		t.Errorf("empty query: total_is_lower_bound/has_more must both be false, got %v/%v",
			resp.TotalIsLowerBound, resp.HasMore)
	}
}

// --- advanced mode -------------------------------------------------------

// TestRecallWindow pins the block-quantised window: the base block for any page
// inside it, whole blocks beyond, capped at the ceiling.
func TestRecallWindow(t *testing.T) {
	cases := []struct {
		offset, limit, want int
	}{
		{0, 20, recallLimit},
		{480, 20, recallLimit},      // last page fully inside the base block
		{500, 20, 2 * recallLimit},  // first page past it widens by a whole block
		{980, 20, 2 * recallLimit},  // still inside block 2
		{1000, 20, 3 * recallLimit}, // block 3
		{1600, 20, maxRecallLimit},  // block 4 == the ceiling
		{50000, 20, maxRecallLimit}, // way past the ceiling stays clamped
		{-1, 20, recallLimit},       // negative offset is normalised before this
		{0, maxLimit, recallLimit},  // the largest single page still fits block 1
	}
	for _, tc := range cases {
		if got := recallWindow(tc.offset, tc.limit); got != tc.want {
			t.Errorf("recallWindow(%d, %d) = %d, want %d", tc.offset, tc.limit, got, tc.want)
		}
	}
}

// TestAdvancedTotalIsExactWhenRecallFits proves the lower-bound flag is only set
// when the service actually stopped looking.
func TestAdvancedTotalIsExactWhenRecallFits(t *testing.T) {
	f := &fakeQuerier{corpus: 137}
	resp, err := newTestService(f).Search(context.Background(), Request{Query: "golang", Mode: "advanced", Limit: 20})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if resp.Total == nil || *resp.Total != 137 {
		t.Fatalf("total = %s, want 137", totalOf(resp))
	}
	if resp.TotalIsLowerBound {
		t.Errorf("recall came back under the window; total is exact, flag must be false")
	}
	if got := f.recallCalls[0].Lim; got != int32(recallLimit+1) {
		t.Errorf("recall LIMIT = %d, want window+1 (the overflow probe)", got)
	}
}

// TestAdvancedTotalIsLowerBoundWhenRecallCaps proves an over-full recall is
// reported honestly: the window size, marked as a lower bound — never a
// fabricated corpus-wide total.
func TestAdvancedTotalIsLowerBoundWhenRecallCaps(t *testing.T) {
	f := &fakeQuerier{corpus: 10_000}
	resp, err := newTestService(f).Search(context.Background(), Request{Query: "golang", Mode: "advanced", Limit: 20})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if resp.Total == nil || *resp.Total != recallLimit {
		t.Fatalf("total = %s, want the recall window size %d", totalOf(resp), recallLimit)
	}
	if !resp.TotalIsLowerBound {
		t.Errorf("recall hit its cap; total_is_lower_bound must be true")
	}
	if !resp.HasMore {
		t.Errorf("has_more must be true — the window can still grow")
	}
	// The probe row must never reach the client as a result.
	if len(resp.IDs) != 20 {
		t.Errorf("ids = %d, want 20", len(resp.IDs))
	}
}

// TestAdvancedPagingCrossesTheRecallBoundary is the regression test for the bug:
// a client paging forward past the base recall block used to get an empty page
// indistinguishable from the end of results. Every page up to the corpus size
// must now be non-empty, and the walk must terminate.
func TestAdvancedPagingCrossesTheRecallBoundary(t *testing.T) {
	const corpus = 1234
	const limit = 20
	f := &fakeQuerier{corpus: corpus}
	svc := newTestService(f)

	seen := 0
	pages := 0
	for offset := 0; ; offset += limit {
		resp, err := svc.Search(context.Background(), Request{
			Query: "golang", Mode: "advanced", Limit: limit, Offset: offset,
		})
		if err != nil {
			t.Fatalf("offset %d: %v", offset, err)
		}
		pages++
		if pages > 200 {
			t.Fatalf("paging did not terminate — has_more never went false")
		}
		if len(resp.IDs) == 0 {
			t.Fatalf("offset %d returned an EMPTY page while has_more was true on the "+
				"previous page — this is the recall cliff", offset)
		}
		seen += len(resp.IDs)
		if !resp.HasMore {
			break
		}
	}
	if seen != corpus {
		t.Errorf("walked %d results, want the whole %d-document corpus", seen, corpus)
	}
}

// TestAdvancedBoundaryPageIsNotTheEnd nails the exact off-by-block that used to
// break: the last page of the base block must say "more", and the first page
// past it must actually deliver.
func TestAdvancedBoundaryPageIsNotTheEnd(t *testing.T) {
	f := &fakeQuerier{corpus: 1234}
	svc := newTestService(f)

	last := mustSearch(t, svc, Request{Query: "golang", Mode: "advanced", Limit: 20, Offset: recallLimit - 20})
	if len(last.IDs) != 20 {
		t.Fatalf("last page of the base block: ids = %d, want 20", len(last.IDs))
	}
	if !last.HasMore {
		t.Errorf("last page of the base block must report has_more")
	}

	next := mustSearch(t, svc, Request{Query: "golang", Mode: "advanced", Limit: 20, Offset: recallLimit})
	if len(next.IDs) != 20 {
		t.Errorf("first page past the base block: ids = %d, want 20 (this was the cliff)", len(next.IDs))
	}
	// The window widened to two blocks, which the 1234-doc corpus still overflows,
	// so the total is the widened window and still a lower bound.
	if next.Total == nil || *next.Total != 2*recallLimit {
		t.Errorf("total = %s, want the widened window %d", totalOf(next), 2*recallLimit)
	}
	if !next.TotalIsLowerBound {
		t.Errorf("the corpus still overflows the widened window; total_is_lower_bound must be true")
	}

	// A corpus that FITS the widened window reports an exact total there.
	small := newTestService(&fakeQuerier{corpus: 700})
	fits := mustSearch(t, small, Request{Query: "golang", Mode: "advanced", Limit: 20, Offset: recallLimit})
	if fits.Total == nil || *fits.Total != 700 {
		t.Errorf("total = %s, want the exact 700-doc corpus", totalOf(fits))
	}
	if fits.TotalIsLowerBound {
		t.Errorf("the widened window covers the 700-doc corpus; total is exact")
	}
}

// totalOf renders Response.Total for failure messages ("<omitted>" when nil).
func totalOf(r Response) string {
	if r.Total == nil {
		return "<omitted>"
	}
	return strconv.Itoa(*r.Total)
}

// TestAdvancedStopsHonestlyAtTheCeiling proves that at the hard recall ceiling
// paging stops (has_more false, so no client loops on empty pages) while
// total_is_lower_bound stays true to say the corpus was NOT exhausted.
func TestAdvancedStopsHonestlyAtTheCeiling(t *testing.T) {
	f := &fakeQuerier{corpus: 50_000}
	svc := newTestService(f)

	resp := mustSearch(t, svc, Request{Query: "golang", Mode: "advanced", Limit: 20, Offset: maxRecallLimit - 20})
	if len(resp.IDs) != 20 {
		t.Fatalf("last servable page: ids = %d, want 20", len(resp.IDs))
	}
	if resp.HasMore {
		t.Errorf("has_more must be false at the ceiling — further pages cannot be served")
	}
	if !resp.TotalIsLowerBound {
		t.Errorf("total_is_lower_bound must stay true: the corpus was not exhausted")
	}
	if resp.Total == nil || *resp.Total != maxRecallLimit {
		t.Errorf("total = %s, want the ceiling %d", totalOf(resp), maxRecallLimit)
	}
	// And the recall never asks Postgres for more than the ceiling (+ the probe).
	if got := f.recallCalls[0].Lim; got != int32(maxRecallLimit+1) {
		t.Errorf("recall LIMIT = %d, want the ceiling+1", got)
	}
}

// TestAdvancedEmptyRecallIsExactZero proves a no-hit advanced query reports an
// exact zero rather than an unqualified one.
func TestAdvancedEmptyRecallIsExactZero(t *testing.T) {
	f := &fakeQuerier{corpus: 0}
	resp := mustSearch(t, newTestService(f), Request{Query: "golang", Mode: "advanced"})
	if resp.Total == nil || *resp.Total != 0 {
		t.Errorf("total = %s, want an exact 0", totalOf(resp))
	}
	if resp.TotalIsLowerBound || resp.HasMore {
		t.Errorf("empty recall: both flags must be false, got %v/%v", resp.TotalIsLowerBound, resp.HasMore)
	}
}

func mustSearch(t *testing.T, svc *Service, req Request) Response {
	t.Helper()
	resp, err := svc.Search(context.Background(), req)
	if err != nil {
		t.Fatalf("search %+v: %v", req, err)
	}
	return resp
}
