package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vidra/vidra-search/internal/moderation"
	"github.com/vidra/vidra-search/internal/store/sqlcgen"
)

// fakeBanQuerier is an in-memory moderation.Querier so the handler tests exercise
// routing, auth, normalization and status codes without a database.
type fakeBanQuerier struct {
	banned   []string
	unbanned []string
	rows     []sqlcgen.ListBannedSuggestionsRow
	err      error
	lastList sqlcgen.ListBannedSuggestionsParams
}

func (f *fakeBanQuerier) BanSuggestion(_ context.Context, nq string) error {
	if f.err != nil {
		return f.err
	}
	f.banned = append(f.banned, nq)
	return nil
}

func (f *fakeBanQuerier) UnbanSuggestion(_ context.Context, nq string) error {
	if f.err != nil {
		return f.err
	}
	f.unbanned = append(f.unbanned, nq)
	return nil
}

func (f *fakeBanQuerier) ListBannedSuggestions(_ context.Context, arg sqlcgen.ListBannedSuggestionsParams) ([]sqlcgen.ListBannedSuggestionsRow, error) {
	f.lastList = arg
	if f.err != nil {
		return nil, f.err
	}
	return f.rows, nil
}

// newModerationServer builds a Server whose only wired service is moderation.
func newModerationServer(q moderation.Querier) *Server {
	return New(testConfig(), nil, nil, nil, nil, Services{Moderation: moderation.NewService(q)})
}

// signedDo drives one signed request against the server and returns the recorder.
// The HMAC is computed over the DECODED path, matching how vidra-core signs.
func signedDo(srv *Server, method, target, decodedPath string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	req.Header.Set(internalAuthHeader, BuildInternalAuthHeader(testSecret, time.Now().Unix(), method, decodedPath))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestBanSuggestionRoute(t *testing.T) {
	q := &fakeBanQuerier{}
	srv := newModerationServer(q)

	const path = "/internal/v1/suggestions/bans/Buy%20Cheap%20Followers"
	const decoded = "/internal/v1/suggestions/bans/Buy Cheap Followers"
	rec := signedDo(srv, http.MethodPut, path, decoded)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT ban = %d %s, want 200", rec.Code, rec.Body.String())
	}
	var body moderation.BanResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode ban response: %v (%s)", err, rec.Body.String())
	}
	// The handler must ban the NORMALIZED key, not the raw path text — otherwise
	// a ban placed on the display form would never match the aggregate row.
	if body.NormalizedQuery != "buy cheap followers" || !body.Banned {
		t.Errorf("ban response = %+v, want normalized_query=%q banned=true", body, "buy cheap followers")
	}
	if len(q.banned) != 1 || q.banned[0] != "buy cheap followers" {
		t.Errorf("store saw %v, want [buy cheap followers]", q.banned)
	}
}

func TestBanSuggestionRejectsEmptyQuery(t *testing.T) {
	q := &fakeBanQuerier{}
	srv := newModerationServer(q)
	// A path segment that normalizes away to nothing must not ban the empty key.
	const path = "/internal/v1/suggestions/bans/%20%20"
	const decoded = "/internal/v1/suggestions/bans/  "
	rec := signedDo(srv, http.MethodPut, path, decoded)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("blank query ban = %d %s, want 422", rec.Code, rec.Body.String())
	}
	if len(q.banned) != 0 {
		t.Errorf("store must not be called for a blank query, saw %v", q.banned)
	}
}

func TestUnbanSuggestionRoute(t *testing.T) {
	q := &fakeBanQuerier{}
	srv := newModerationServer(q)

	const path = "/internal/v1/suggestions/bans/buy%20cheap%20followers"
	const decoded = "/internal/v1/suggestions/bans/buy cheap followers"
	rec := signedDo(srv, http.MethodDelete, path, decoded)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE ban = %d %s, want 204", rec.Code, rec.Body.String())
	}
	if len(q.unbanned) != 1 || q.unbanned[0] != "buy cheap followers" {
		t.Errorf("store saw %v, want [buy cheap followers]", q.unbanned)
	}
	// Idempotent: unbanning again is still a 204, so a retrying core never sees
	// a spurious failure.
	if rec2 := signedDo(srv, http.MethodDelete, path, decoded); rec2.Code != http.StatusNoContent {
		t.Errorf("repeat DELETE = %d, want 204", rec2.Code)
	}
}

func TestListSuggestionBansRoute(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	q := &fakeBanQuerier{rows: []sqlcgen.ListBannedSuggestionsRow{{
		NormalizedQuery: "buy cheap followers",
		DisplayQuery:    "Buy Cheap Followers",
		TotalCount:      42,
		DistinctUsers:   7,
		FirstSeen:       now,
		LastSeen:        now,
	}}}
	srv := newModerationServer(q)

	const path = "/internal/v1/suggestions/bans?limit=5&offset=10"
	rec := signedDo(srv, http.MethodGet, path, "/internal/v1/suggestions/bans")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET bans = %d %s, want 200", rec.Code, rec.Body.String())
	}
	var body moderation.ListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode list response: %v (%s)", err, rec.Body.String())
	}
	if len(body.Entries) != 1 || body.Entries[0].NormalizedQuery != "buy cheap followers" {
		t.Fatalf("entries = %+v, want the one banned query", body.Entries)
	}
	if body.Entries[0].Query != "Buy Cheap Followers" || body.Entries[0].DistinctUsers != 7 {
		t.Errorf("entry = %+v, want the display form and its counts for review", body.Entries[0])
	}
	if body.Limit != 5 || body.Offset != 10 {
		t.Errorf("paging echoed as limit=%d offset=%d, want 5/10", body.Limit, body.Offset)
	}
	if q.lastList.Lim != 5 || q.lastList.Off != 10 {
		t.Errorf("store paging params = %+v, want lim=5 off=10", q.lastList)
	}
}

// TestSuggestionBanRoutesRejectBadSignature pins that the ban surface sits behind
// the same HMAC gate as every other /internal/v1 route: a forged signature is a
// 401 and the store is never touched.
func TestSuggestionBanRoutesRejectBadSignature(t *testing.T) {
	q := &fakeBanQuerier{}
	srv := newModerationServer(q)

	cases := []struct{ method, target string }{
		{http.MethodPut, "/internal/v1/suggestions/bans/spam"},
		{http.MethodDelete, "/internal/v1/suggestions/bans/spam"},
		{http.MethodGet, "/internal/v1/suggestions/bans"},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(tc.method, tc.target, nil)
		// Correct construction, wrong secret.
		req.Header.Set(internalAuthHeader,
			BuildInternalAuthHeader("a-completely-different-secret-32-bytes!!", time.Now().Unix(), tc.method, tc.target))
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s with a bad signature = %d, want 401", tc.method, tc.target, rec.Code)
		}
	}
	if len(q.banned)+len(q.unbanned) != 0 {
		t.Errorf("unauthenticated requests must never reach the store, saw ban=%v unban=%v", q.banned, q.unbanned)
	}
}

// TestBanSuggestionStoreFailureIs500 proves a store error surfaces as the standard
// error envelope rather than a partial success.
func TestBanSuggestionStoreFailureIs500(t *testing.T) {
	q := &fakeBanQuerier{err: errors.New("boom")}
	srv := newModerationServer(q)
	rec := signedDo(srv, http.MethodPut, "/internal/v1/suggestions/bans/spam", "/internal/v1/suggestions/bans/spam")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("store failure = %d %s, want 500", rec.Code, rec.Body.String())
	}
}
