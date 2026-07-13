package api

import (
	"net/http"
	"net/http/httptest"
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

func TestInternalAuthMalformed(t *testing.T) {
	for _, h := range []string{"garbage", "v2:123:abc", "v1:notanumber:abc", "v1:123"} {
		if rec := invokeAuth(t, h); rec.Code != http.StatusUnauthorized {
			t.Fatalf("malformed header %q should 401, got %d", h, rec.Code)
		}
	}
}
