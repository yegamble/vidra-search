package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
)

// errorEnvelope drives one request and decodes the JSON error envelope, which
// is the contract vidra-core's client parses.
func errorEnvelope(t *testing.T, srv *Server, req *http.Request) (int, ErrorResponse) {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	var body ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal %q: %v", rec.Body.String(), err)
	}
	return rec.Code, body
}

// newErrorServer registers a test-only route that fails with err, so the
// central error handler can be exercised without a live dependency.
func newErrorServer(t *testing.T, path string, err error) *Server {
	t.Helper()
	srv := New(testConfig(), nil, nil, nil, nil, Services{})
	srv.Handler().GET(path, func(echo.Context) error { return err })
	return srv
}

func TestErrorEnvelopeConflict(t *testing.T) {
	srv := newErrorServer(t, "/test/conflict",
		echo.NewHTTPError(http.StatusConflict, "that name is taken"))
	code, body := errorEnvelope(t, srv, httptest.NewRequest(http.MethodGet, "/test/conflict", nil))

	if code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", code)
	}
	if body.Error.Code != "conflict" {
		t.Errorf("code = %q, want conflict", body.Error.Code)
	}
	if body.Error.Message != "that name is taken" {
		t.Errorf("message = %q, want the handler's", body.Error.Message)
	}
}

// 501 is a documented, client-safe answer, so its code is stamped before the
// generic 5xx message scrub — otherwise the client gets "an unexpected error
// occurred" for something entirely expected.
func TestErrorEnvelopeNotImplemented(t *testing.T) {
	srv := newErrorServer(t, "/test/not-implemented",
		echo.NewHTTPError(http.StatusNotImplemented, "that ranker is not wired up"))
	code, body := errorEnvelope(t, srv, httptest.NewRequest(http.MethodGet, "/test/not-implemented", nil))

	if code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", code)
	}
	if body.Error.Code != "not_implemented" {
		t.Errorf("code = %q, want not_implemented", body.Error.Code)
	}
	if body.Error.Message != "that ranker is not wired up" {
		t.Errorf("message = %q, want the handler's — the 5xx scrub must not swallow it", body.Error.Message)
	}
}

// A plain 500 IS scrubbed: nothing a handler said about an unexpected failure
// reaches the client.
func TestErrorEnvelopeInternalIsScrubbed(t *testing.T) {
	srv := newErrorServer(t, "/test/boom",
		echo.NewHTTPError(http.StatusInternalServerError, "dsn=postgres://user:hunter2@db/search"))
	code, body := errorEnvelope(t, srv, httptest.NewRequest(http.MethodGet, "/test/boom", nil))

	if code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", code)
	}
	if body.Error.Code != "internal_error" {
		t.Errorf("code = %q, want internal_error", body.Error.Code)
	}
	if body.Error.Message != "an unexpected error occurred" {
		t.Errorf("message = %q leaked handler detail", body.Error.Message)
	}
}

// The HMAC middleware answers before the handler chain, so its envelope comes
// from writeError rather than the central handler; it has to look the same.
func TestErrorEnvelopeUnauthorized(t *testing.T) {
	srv := New(testConfig(), nil, nil, nil, nil, Services{})
	code, body := errorEnvelope(t, srv,
		httptest.NewRequest(http.MethodGet, "/internal/v1/search?q=cats", nil))

	if code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", code)
	}
	if body.Error.Code != "unauthorized" {
		t.Errorf("code = %q, want unauthorized", body.Error.Code)
	}
	if body.Error.Message == "" {
		t.Error("message is empty; the envelope always carries one")
	}
}

func TestCodeForStatus(t *testing.T) {
	cases := map[int]string{
		http.StatusBadRequest:            "bad_request",
		http.StatusUnauthorized:          "unauthorized",
		http.StatusForbidden:             "forbidden",
		http.StatusNotFound:              "not_found",
		http.StatusMethodNotAllowed:      "method_not_allowed",
		http.StatusConflict:              "conflict",
		http.StatusRequestEntityTooLarge: "request_entity_too_large",
		http.StatusUnprocessableEntity:   "unprocessable_entity",
		http.StatusTooManyRequests:       "rate_limited",
		http.StatusNotImplemented:        "not_implemented",
		http.StatusServiceUnavailable:    "service_unavailable",
		http.StatusInternalServerError:   "internal_error",
		// Unmapped statuses fall back to the class.
		http.StatusTeapot:           "client_error",
		http.StatusBadGateway:       "server_error",
		http.StatusMovedPermanently: "error",
	}
	for status, want := range cases {
		if got := codeForStatus(status); got != want {
			t.Errorf("codeForStatus(%d) = %q, want %q", status, got, want)
		}
	}
}
