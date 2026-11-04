//go:build integration

package store_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vidra/vidra-search/internal/api"
	"github.com/vidra/vidra-search/internal/config"
)

func TestCatalogHTTPPreInitializationRequiresAbsentReadFence(t *testing.T) {
	env := newTestEnv(t)
	v := video("restore legacy projection")
	ingest(t, env, upsertEnvelope(t, v))
	status, err := env.store.ReadCatalogStatus(context.Background(), "local")
	if err != nil || status.Epoch != nil {
		t.Fatalf("fixture is not before initialization: %+v/%v", status, err)
	}
	secret := "synthetic-restore-secret-at-least-32-bytes"
	srv := api.New(&config.Config{InternalSecret: secret, HTTPBodyLimit: "2M", HTTPRequestTimeout: 30 * time.Second},
		nil, nil, env.store, env.cache, api.Services{Events: env.events, Search: env.search, Suggest: env.sugg, Rec: env.rec})
	paths := []string{"/internal/v1/search?q=restore", "/internal/v1/suggestions?q=re", "/internal/v1/recommendations/home",
		"/internal/v1/recommendations/related?video_id=" + v.ID.String(), "/internal/v1/suggestions"}
	for _, path := range paths {
		for _, headers := range []http.Header{
			nil,
			{"X-Vidra-Catalog-Epoch": {uuid.NewString()}, "X-Vidra-Catalog-Generation": {"7"}},
			{"X-Vidra-Catalog-Epoch": {uuid.NewString()}},
			{"X-Vidra-Catalog-Generation": {"7"}},
			{"X-Vidra-Catalog-Epoch": {""}},
			{"X-Vidra-Catalog-Generation": {""}},
			{"X-Vidra-Catalog-Epoch": {""}, "X-Vidra-Catalog-Generation": {""}},
		} {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			for name, values := range headers {
				req.Header[name] = values
			}
			req.Header.Set("X-Vidra-Internal-Auth", api.BuildInternalAuthHeader(secret, time.Now().Unix(), req.Method, req.URL.Path))
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)
			want := http.StatusServiceUnavailable
			if headers == nil {
				want = http.StatusOK
			}
			if rec.Code != want {
				t.Fatalf("%s headers=%v status=%d want%d: %s", path, headers, rec.Code, want, rec.Body.String())
			}
			if headers == nil && path == paths[0] && !strings.Contains(rec.Body.String(), v.ID.String()) {
				t.Fatal("unfenced legacy search did not exercise the seeded projection")
			}
		}
	}
}
