//go:build integration

package store_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vidra/vidra-search/internal/api"
	"github.com/vidra/vidra-search/internal/catalog"
	"github.com/vidra/vidra-search/internal/config"
)

func TestCatalogHTTPAuthenticatedFullRoundtripAndGuardedReads(t *testing.T) {
	env := newTestEnv(t)
	secret := "synthetic-catalog-secret-at-least-32-bytes"
	srv := api.New(&config.Config{InternalSecret: secret, HTTPBodyLimit: "2M", HTTPRequestTimeout: 30 * time.Second},
		nil, nil, env.store, env.cache, api.Services{Events: env.events, Search: env.search, Suggest: env.sugg, Rec: env.rec})
	epoch := uuid.New()
	encode := func(value any) []byte {
		t.Helper()
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	request := func(method, path string, body []byte, generation string, signed bool, want int) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, bytes.NewReader(body))
		if signed {
			req.Header.Set("X-Vidra-Internal-Auth", api.BuildInternalAuthHeader(secret, time.Now().Unix(), method, req.URL.Path))
		}
		if generation != "" {
			req.Header.Set("X-Vidra-Catalog-Epoch", epoch.String())
			req.Header.Set("X-Vidra-Catalog-Generation", generation)
		}
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("%s %s status=%d want%d: %s", method, path, rec.Code, want, rec.Body.String())
		}
		return rec
	}
	status := func() catalog.Status {
		t.Helper()
		rec := request("GET", "/internal/v2/catalog/status?scope=local", nil, "", true, 200)
		var got catalog.Status
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	request("GET", "/internal/v2/catalog/status?scope=local", nil, "", false, http.StatusUnauthorized)
	bootstrap := encode(map[string]any{"scope": "local", "epoch": epoch, "expected_epoch": nil})
	request("PUT", "/internal/v2/catalog/epoch", bootstrap, "", false, http.StatusUnauthorized)
	if got := status(); got.Epoch != nil || got.Ready || got.CompletedGeneration != "0" {
		t.Fatalf("unbootstrapped authority: %+v", got)
	}
	request("PUT", "/internal/v2/catalog/epoch", bootstrap, "", true, 204)
	request("GET", "/internal/v1/search?q=protocol", nil, "", true, 503)
	v := video("protocol roundtrip")
	documents := encode([]map[string]any{{"video": v, "eligible": true, "suppressed_reason": nil}})
	_, hash, _ := catalog.DocumentsDigest(documents)
	membership, _ := catalog.MembershipDigest([]uuid.UUID{v.ID})
	content, _ := catalog.PagesDigest([]catalog.PageDigest{{PageIndex: 0, ContentSHA256: hash}})
	page := rawEnvelope(t, catalog.TypeSnapshotPage, map[string]any{"scope": "local", "epoch": epoch,
		"generation": "1", "page_index": 0, "documents": json.RawMessage(documents), "content_sha256": hash})
	end := rawEnvelope(t, catalog.TypeSnapshotEnd, map[string]any{"scope": "local", "epoch": epoch,
		"generation": "1", "page_count": 1, "document_count": 1, "membership_sha256": membership, "pages_sha256": content})
	page.SchemaVersion, end.SchemaVersion = 2, 2
	body := encode(map[string]any{"events": []any{end}})
	exact := append(append([]byte(nil), body...), bytes.Repeat([]byte(" "), catalog.MaxRequestBytes-len(body))...)
	request("POST", "/internal/v2/catalog/events", exact, "", true, 200)
	request("POST", "/internal/v2/catalog/events", append(exact, ' '), "", true, 413)
	if got := status(); got.Ready || got.HighestObservedGeneration != "1" || got.StagedSnapshots != 1 {
		t.Fatalf("end receipt became authority: %+v", got)
	}
	request("POST", "/internal/v2/catalog/events", encode(map[string]any{"events": []any{page}}), "", true, 200)
	if got := status(); !got.Ready || got.CompletedGeneration != "1" || got.StagedSnapshots != 0 || got.CompletedManifest.MembershipSHA256 != membership {
		t.Fatalf("completed authority missing: %+v", got)
	}
	paths := []string{"/internal/v1/search?q=protocol", "/internal/v1/suggestions?q=pr",
		"/internal/v1/recommendations/home", "/internal/v1/recommendations/related?video_id=" + v.ID.String()}
	for _, path := range paths {
		request("GET", path, nil, "", true, 503)
		request("GET", path, nil, "1", true, 200)
	}
	empty, _ := catalog.MembershipDigest(nil)
	end = rawEnvelope(t, catalog.TypeSnapshotEnd, map[string]any{"scope": "local", "epoch": epoch,
		"generation": "2", "page_count": 0, "document_count": 0, "membership_sha256": empty, "pages_sha256": empty})
	end.SchemaVersion = 2
	request("POST", "/internal/v2/catalog/events", encode(map[string]any{"events": []any{end}}), "", true, 200)
	for _, path := range paths {
		request("GET", path, nil, "1", true, 503)
	}
	if got := request("GET", paths[0], nil, "2", true, 200); strings.Contains(got.Body.String(), v.ID.String()) {
		t.Fatal("empty full did not suppress old local membership")
	}
}
