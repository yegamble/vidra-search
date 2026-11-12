package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"

	"github.com/vidra/vidra-search/internal/catalog"
	"github.com/vidra/vidra-search/internal/store"
)

type epochController struct {
	catalogReader
	calls    int
	epoch    uuid.UUID
	expected *uuid.UUID
	err      error
}

func (r *epochController) SetCatalogEpoch(_ context.Context, scope string, epoch uuid.UUID, expected *uuid.UUID) error {
	r.calls++
	r.epoch, r.expected = epoch, expected
	return r.err
}

func callCatalogHandler(srv *Server, method, path, body string, handler echo.HandlerFunc) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	c := srv.Handler().NewContext(req, rec)
	if err := handler(c); err != nil {
		srv.httpErrorHandler(c, err)
	}
	return rec
}

func TestCatalogEpochHandlerIsStrictBoundedAndUsesExplicitCAS(t *testing.T) {
	epoch, predecessor := uuid.New(), uuid.New()
	valid := `{"scope":"local","epoch":"` + epoch.String() + `","expected_epoch":null}`
	db := &epochController{}
	srv := New(testConfig(), nil, nil, db, nil, Services{})
	exact := valid + strings.Repeat(" ", catalog.MaxRequestBytes-len(valid))
	if rec := callCatalogHandler(srv, "PUT", "/", exact, srv.handleCatalogEpoch); rec.Code != http.StatusNoContent || db.calls != 1 || db.epoch != epoch || db.expected != nil {
		t.Fatalf("initial CAS failed: status=%d controller=%+v", rec.Code, db)
	}
	reset := strings.Replace(valid, "null", `"`+predecessor.String()+`"`, 1)
	if rec := callCatalogHandler(srv, "PUT", "/", reset, srv.handleCatalogEpoch); rec.Code != 204 || db.expected == nil || *db.expected != predecessor {
		t.Fatalf("reset CAS lost predecessor: status=%d controller=%+v", rec.Code, db)
	}
	for _, bad := range []string{
		`{}`, valid + `{}`, strings.Replace(valid, `"scope":"local"`, `"scope":"remote"`, 1),
		strings.Replace(valid, `"epoch":`, `"Epoch":`, 1),
		strings.Replace(valid, `"scope":"local"`, `"scope":"local","scope":"local"`, 1),
		strings.Replace(valid, epoch.String(), uuid.Nil.String(), 1),
		strings.Replace(reset, predecessor.String(), uuid.Nil.String(), 1),
	} {
		if rec := callCatalogHandler(srv, "PUT", "/", bad, srv.handleCatalogEpoch); rec.Code != 422 || db.calls != 2 {
			t.Fatalf("invalid shape called CAS: status=%d calls=%d body=%s", rec.Code, db.calls, bad)
		}
	}
	if rec := callCatalogHandler(srv, "PUT", "/", exact+" ", srv.handleCatalogEpoch); rec.Code != 413 || db.calls != 2 {
		t.Fatalf("over-budget CAS reached store: %d/%d", rec.Code, db.calls)
	}
	db.err = store.ErrCatalogConflict
	if rec := callCatalogHandler(srv, "PUT", "/", valid, srv.handleCatalogEpoch); rec.Code != 409 {
		t.Fatalf("CAS conflict status=%d", rec.Code)
	}
}

func TestCatalogStatusAndEventsHandlersRefuseInvalidTransport(t *testing.T) {
	db := &catalogReader{statuses: []catalog.Status{{ProtocolVersion: 2, Scope: "local", CompletedGeneration: "0"}}}
	srv := New(testConfig(), nil, nil, db, nil, Services{})
	if rec := callCatalogHandler(srv, "GET", "/?scope=local", "", srv.handleCatalogStatus); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"completed_generation":"0"`) {
		t.Fatalf("status wire=%d/%s", rec.Code, rec.Body.String())
	}
	if rec := callCatalogHandler(srv, "GET", "/?scope=remote", "", srv.handleCatalogStatus); rec.Code != 422 {
		t.Fatalf("unsupported scope status=%d", rec.Code)
	}
	for _, body := range []string{`{}`, `{"events":[null]}`, `{"events":[]} {}`} {
		if rec := callCatalogHandler(srv, "POST", "/", body, srv.handleCatalogEvents); rec.Code != 422 {
			t.Fatalf("invalid batch reached service: %d", rec.Code)
		}
	}
	if rec := callCatalogHandler(srv, "POST", "/", strings.Repeat(" ", catalog.MaxRequestBytes+1), srv.handleCatalogEvents); rec.Code != 413 {
		t.Fatalf("whole batch limit status=%d", rec.Code)
	}
}
