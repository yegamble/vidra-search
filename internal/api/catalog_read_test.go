package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vidra/vidra-search/internal/catalog"
	"github.com/vidra/vidra-search/internal/suggest"
)

type catalogReader struct {
	statuses []catalog.Status
	reads    int
	err      error
}

func (*catalogReader) Ping(context.Context) error { return nil }
func (r *catalogReader) ReadCatalogStatus(context.Context, string) (catalog.Status, error) {
	s := r.statuses[min(r.reads, len(r.statuses)-1)]
	r.reads++
	return s, r.err
}

func TestCatalogReadFenceRejectsOldReadersOnAllCatalogRoutes(t *testing.T) {
	epoch := uuid.New()
	for _, path := range []string{"/internal/v1/search?q=ca", "/internal/v1/suggestions?q=ca",
		"/internal/v1/recommendations/home", "/internal/v1/recommendations/related?video_id=" + uuid.NewString()} {
		t.Run(path, func(t *testing.T) {
			for _, ready := range []bool{false, true} {
				db := &catalogReader{statuses: []catalog.Status{{Epoch: &epoch, Ready: ready, CompletedGeneration: "7"}}}
				srv := New(testConfig(), nil, nil, db, nil, Services{})
				req := httptest.NewRequest(http.MethodGet, path, nil)
				req.Header.Set(internalAuthHeader, BuildInternalAuthHeader(testSecret, time.Now().Unix(), req.Method, req.URL.Path))
				rec := httptest.NewRecorder()
				srv.Handler().ServeHTTP(rec, req)
				if rec.Code != http.StatusServiceUnavailable {
					t.Fatalf("ready=%v unsupported old reader status=%d, want503", ready, rec.Code)
				}
			}
		})
	}
}

func TestCatalogReadFenceChecksAfterComputationBeforeSuccess(t *testing.T) {
	epoch, nextEpoch := uuid.New(), uuid.New()
	ready := catalog.Status{Epoch: &epoch, Ready: true, CompletedGeneration: "7"}
	next := ready
	next.CompletedGeneration = "8"
	reset := catalog.Status{Epoch: &nextEpoch, Ready: true, CompletedGeneration: "1"}
	foreign := ready
	foreign.ForeignEligible = true
	for _, tc := range []struct {
		name       string
		states     []catalog.Status
		epoch, gen string
		err        error
		want       int
	}{
		{"legacy", []catalog.Status{{}, {}}, "", "", nil, 200},
		{"matching", []catalog.Status{ready, ready}, epoch.String(), "7", nil, 200},
		{"generation changed", []catalog.Status{ready, next}, epoch.String(), "7", nil, 503},
		{"epoch changed", []catalog.Status{ready, reset}, epoch.String(), "7", nil, 503},
		{"foreign eligibility changed", []catalog.Status{ready, foreign}, epoch.String(), "7", nil, 503},
		{"initialization raced", []catalog.Status{{}, ready}, "", "", nil, 503},
		{"noncanonical generation", []catalog.Status{ready}, epoch.String(), "07", nil, 503},
		{"wrong epoch", []catalog.Status{ready}, nextEpoch.String(), "7", nil, 503},
		{"database unavailable", []catalog.Status{ready}, epoch.String(), "7", errors.New("unavailable"), 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := &catalogReader{statuses: tc.states, err: tc.err}
			// Empty suggestions finish without SQL, isolating the actual HTTP fence.
			srv := New(testConfig(), nil, nil, db, nil, Services{Suggest: suggest.NewService(nil, nil, nil, nil, nil, nil)})
			req := httptest.NewRequest(http.MethodGet, "/internal/v1/suggestions", nil)
			req.Header.Set(internalAuthHeader, BuildInternalAuthHeader(testSecret, time.Now().Unix(), req.Method, req.URL.Path))
			req.Header.Set("X-Vidra-Catalog-Epoch", tc.epoch)
			req.Header.Set("X-Vidra-Catalog-Generation", tc.gen)
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status=%d, want%d: %s", rec.Code, tc.want, rec.Body.String())
			}
			if tc.want == 200 && db.reads != 2 {
				t.Fatalf("success did not check both fences: %d reads", db.reads)
			}
		})
	}
}
