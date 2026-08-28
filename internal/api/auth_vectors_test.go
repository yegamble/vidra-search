package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
)

// internalAuthVectorsPath is the shared conformance fixture for the v1
// internal-auth protocol. vidra-core implements the SIGNING half of this
// protocol independently, so the two repos agree only by accident unless
// something pins the wire bytes. This file is that pin: it is generated from
// the implementation below (the canonical verifier) and is meant to be copied
// BYTE-IDENTICALLY into vidra-core's test suite, where the signer must
// reproduce every expected_header.
const internalAuthVectorsPath = "testdata/internal_auth_vectors.json"

type internalAuthVector struct {
	Name           string `json:"name"`
	Note           string `json:"note"`
	Secret         string `json:"secret"`
	TS             int64  `json:"ts"`
	Method         string `json:"method"`
	Path           string `json:"path"`
	ExpectedSig    string `json:"expected_sig"`
	ExpectedHeader string `json:"expected_header"`
}

type internalAuthVectorFile struct {
	Protocol string               `json:"protocol"`
	Vectors  []internalAuthVector `json:"vectors"`
}

func loadInternalAuthVectors(t *testing.T) internalAuthVectorFile {
	t.Helper()
	buf, err := os.ReadFile(internalAuthVectorsPath)
	if err != nil {
		t.Fatalf("read %s: %v", internalAuthVectorsPath, err)
	}
	var f internalAuthVectorFile
	if err := json.Unmarshal(buf, &f); err != nil {
		t.Fatalf("parse %s: %v", internalAuthVectorsPath, err)
	}
	if f.Protocol != "v1" {
		t.Fatalf("protocol = %q, want v1", f.Protocol)
	}
	if len(f.Vectors) == 0 {
		t.Fatal("no vectors")
	}
	return f
}

// A change to either function that moves any of these bytes is a protocol
// break: every already-deployed vidra-core starts 401ing against the new
// search, and no test in EITHER repo would otherwise say so.
func TestInternalAuthGoldenVectors(t *testing.T) {
	for _, v := range loadInternalAuthVectors(t).Vectors {
		t.Run(v.Name, func(t *testing.T) {
			if got := InternalSignature(v.Secret, v.TS, v.Method, v.Path); got != v.ExpectedSig {
				t.Errorf("InternalSignature = %s, want %s\n%s", got, v.ExpectedSig, v.Note)
			}
			if got := BuildInternalAuthHeader(v.Secret, v.TS, v.Method, v.Path); got != v.ExpectedHeader {
				t.Errorf("BuildInternalAuthHeader = %s, want %s\n%s", got, v.ExpectedHeader, v.Note)
			}
		})
	}
}

// The vectors are only worth anything if the header they describe is the one
// the middleware actually accepts, so each is replayed through it. The
// timestamp is the one thing that cannot be frozen — the middleware rejects a
// stale one — so the fixture ts is used for the signature and a fresh one for
// the replay, which is exactly how a real caller behaves.
func TestInternalAuthGoldenVectorsVerify(t *testing.T) {
	for _, v := range loadInternalAuthVectors(t).Vectors {
		t.Run(v.Name, func(t *testing.T) {
			e := echo.New()
			// httptest.NewRequest parses the target, so a decoded path with a
			// space or a multi-byte segment has to be set directly to reach the
			// middleware in the form the signer signed.
			req := httptest.NewRequest(v.Method, "/", nil)
			req.URL.Path = v.Path
			req.Header.Set(internalAuthHeader,
				BuildInternalAuthHeader(v.Secret, time.Now().Unix(), v.Method, v.Path))

			rec := httptest.NewRecorder()
			c := e.NewContext(req, rec)
			h := internalAuth(v.Secret)(func(c echo.Context) error {
				return c.NoContent(http.StatusOK)
			})
			if err := h(c); err != nil {
				t.Fatalf("middleware: %v", err)
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 — the vector's construction must be the one the middleware verifies", rec.Code)
			}
		})
	}
}
