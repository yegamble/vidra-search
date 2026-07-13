package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
)

const testSecret = "test-internal-secret-at-least-32-bytes-long"

// invokeAuth runs the internalAuth middleware against a request and returns the
// recorder. The wrapped handler returns 200 only when auth passes.
func invokeAuth(t *testing.T, header string) *httptest.ResponseRecorder {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/internal/v1/search", nil)
	if header != "" {
		req.Header.Set(internalAuthHeader, header)
	}
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetPath("/internal/v1/search")
	h := internalAuth(testSecret)(func(c echo.Context) error {
		return c.NoContent(http.StatusOK)
	})
	_ = h(c)
	return rec
}

func TestInternalAuthValid(t *testing.T) {
	ts := time.Now().Unix()
	header := BuildInternalAuthHeader(testSecret, ts, http.MethodGet, "/internal/v1/search")
	if rec := invokeAuth(t, header); rec.Code != http.StatusOK {
		t.Fatalf("valid header rejected: %d %s", rec.Code, rec.Body.String())
	}
}

func TestInternalAuthMissing(t *testing.T) {
	if rec := invokeAuth(t, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing header should 401, got %d", rec.Code)
	}
}

func TestInternalAuthExpired(t *testing.T) {
	ts := time.Now().Add(-5 * time.Minute).Unix()
	header := BuildInternalAuthHeader(testSecret, ts, http.MethodGet, "/internal/v1/search")
	if rec := invokeAuth(t, header); rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired header should 401, got %d", rec.Code)
	}
}

func TestInternalAuthWrongSignature(t *testing.T) {
	ts := time.Now().Unix()
	// Signed with a different secret.
	header := BuildInternalAuthHeader("some-other-secret-value-thirty-two-bytes!", ts, http.MethodGet, "/internal/v1/search")
	if rec := invokeAuth(t, header); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong signature should 401, got %d", rec.Code)
	}
}

func TestInternalAuthPathMismatch(t *testing.T) {
	ts := time.Now().Unix()
	// Correct secret + fresh ts, but signed for a different path.
	header := BuildInternalAuthHeader(testSecret, ts, http.MethodGet, "/internal/v1/suggestions")
	if rec := invokeAuth(t, header); rec.Code != http.StatusUnauthorized {
		t.Fatalf("path mismatch should 401, got %d", rec.Code)
	}
}

// TestInternalAuthDecodedPathParam proves the HMAC is verified over the DECODED
// request path (r.URL.Path), so vidra-core can sign the decoded path while sending
// the percent-escaped form on the wire — the case for
// DELETE /internal/v1/users/{id}/search-history/{normalized_query} whose query key
// may contain a space or CJK characters.
func TestInternalAuthDecodedPathParam(t *testing.T) {
	const uid = "11111111-1111-1111-1111-111111111111"
	query := "café 東京" // space + non-ASCII
	decodedPath := "/internal/v1/users/" + uid + "/search-history/" + query
	rawTarget := "/internal/v1/users/" + uid + "/search-history/" + url.PathEscape(query)

	ts := time.Now().Unix()
	// core signs the DECODED path.
	header := BuildInternalAuthHeader(testSecret, ts, http.MethodDelete, decodedPath)

	e := echo.New()
	req := httptest.NewRequest(http.MethodDelete, rawTarget, nil)
	req.Header.Set(internalAuthHeader, header)
	if req.URL.Path != decodedPath {
		t.Fatalf("server should observe the decoded path %q, got %q", decodedPath, req.URL.Path)
	}
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetPath("/internal/v1/users/:user_id/search-history/:normalized_query")
	h := internalAuth(testSecret)(func(c echo.Context) error { return c.NoContent(http.StatusOK) })
	_ = h(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("decoded-path-signed header should validate, got %d %s", rec.Code, rec.Body.String())
	}
	// The escaped-path signature must NOT validate (server never uses EscapedPath).
	badHeader := BuildInternalAuthHeader(testSecret, ts, http.MethodDelete, rawTarget)
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodDelete, rawTarget, nil)
	req2.Header.Set(internalAuthHeader, badHeader)
	c2 := e.NewContext(req2, rec2)
	c2.SetPath("/internal/v1/users/:user_id/search-history/:normalized_query")
	_ = internalAuth(testSecret)(func(c echo.Context) error { return c.NoContent(http.StatusOK) })(c2)
	if rec2.Code == http.StatusOK {
		t.Fatalf("a signature over the escaped path must not validate (server verifies decoded path)")
	}
}

func TestInternalAuthMalformed(t *testing.T) {
	for _, h := range []string{"garbage", "v2:123:abc", "v1:notanumber:abc", "v1:123"} {
		if rec := invokeAuth(t, h); rec.Code != http.StatusUnauthorized {
			t.Fatalf("malformed header %q should 401, got %d", h, rec.Code)
		}
	}
}
