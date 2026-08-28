package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// fakePinger is a test double for a dependency probe.
type fakePinger struct{ err error }

func (f fakePinger) Ping(context.Context) error { return f.err }

// countingPinger is a healthy Pinger that records how many times it was asked.
type countingPinger struct {
	mu sync.Mutex
	n  int
}

func (p *countingPinger) Ping(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.n++
	return nil
}

func (p *countingPinger) calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.n
}

// newHealthServer builds a Server with the given probes and no services; the
// ops routes never touch one.
func newHealthServer(db, rdb Pinger) *Server {
	return New(testConfig(), nil, nil, db, rdb, Services{})
}

// getReadyz drives GET /readyz once and returns the code and the decoded body.
func getReadyz(t *testing.T, srv *Server) (int, readinessResponse) {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	var body readinessResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return rec.Code, body
}

func TestHealthz(t *testing.T) {
	srv := newHealthServer(nil, nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body livenessResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Status != "ok" {
		t.Errorf("status = %q, want ok", body.Status)
	}
}

func TestReadyzAllHealthy(t *testing.T) {
	code, body := getReadyz(t, newHealthServer(fakePinger{}, fakePinger{}))

	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if body.Status != "ok" {
		t.Errorf("status = %q, want ok", body.Status)
	}
	if body.Components["postgres"].Status != "ok" || body.Components["redis"].Status != "ok" {
		t.Errorf("components = %+v, want both ok", body.Components)
	}
}

// Postgres is the one dependency that takes an instance out of rotation: every
// document, aggregate and event lives there, so a replica that cannot reach it
// has nothing to serve.
func TestReadyzPostgresDownIs503(t *testing.T) {
	code, body := getReadyz(t, newHealthServer(fakePinger{err: errors.New("connection refused")}, fakePinger{}))

	if code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", code)
	}
	if body.Status != "unavailable" {
		t.Errorf("status = %q, want unavailable", body.Status)
	}
	if body.Components["postgres"].Status != "down" {
		t.Errorf("postgres status = %q, want down", body.Components["postgres"].Status)
	}
}

// Redis is NOT. It backs caches the services fall back from, so a Redis blip is
// a degradation — and 503ing on it would pull every replica in the fleet out of
// rotation at once, since they all share the same Redis.
func TestReadyzRedisDownStaysInRotation(t *testing.T) {
	code, body := getReadyz(t, newHealthServer(fakePinger{}, fakePinger{err: errors.New("connection refused")}))

	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — a Redis outage must not take this replica out of rotation", code)
	}
	if body.Status != "degraded" {
		t.Errorf("status = %q, want degraded", body.Status)
	}
	// The body still has to say so: a 200 that hid the outage would be worse
	// than a 503.
	if body.Components["redis"].Status != "down" {
		t.Errorf("redis status = %q, want down", body.Components["redis"].Status)
	}
	if body.Components["postgres"].Status != "ok" {
		t.Errorf("postgres status = %q, want ok", body.Components["postgres"].Status)
	}
}

// An unwired dependency is a supported shape, not a fault: "not_configured"
// must never degrade the instance.
func TestReadyzNotConfiguredDoesNotDegrade(t *testing.T) {
	code, body := getReadyz(t, newHealthServer(fakePinger{}, nil))

	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if body.Status != "ok" {
		t.Errorf("status = %q, want ok", body.Status)
	}
	if body.Components["redis"].Status != "not_configured" {
		t.Errorf("redis status = %q, want not_configured", body.Components["redis"].Status)
	}
}

// Draining is the load balancer's cue to stop routing here, and it is answered
// without touching any dependency.
func TestReadyzDraining(t *testing.T) {
	db, rdb := &countingPinger{}, &countingPinger{}
	srv := newHealthServer(db, rdb)

	if code, _ := getReadyz(t, srv); code != http.StatusOK {
		t.Fatalf("status before drain = %d, want 200", code)
	}
	if srv.Draining() {
		t.Fatal("Draining() is true before Drain()")
	}

	srv.Drain()
	if !srv.Draining() {
		t.Fatal("Draining() is false after Drain()")
	}
	before := db.calls()
	code, body := getReadyz(t, srv)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status while draining = %d, want 503", code)
	}
	if body.Status != "draining" {
		t.Errorf("status = %q, want draining", body.Status)
	}
	if got := db.calls(); got != before {
		t.Errorf("draining probe pinged postgres %d extra times; it must spend no pooled connection", got-before)
	}
}

// The probe result is cached, so the cost of answering readiness does not scale
// with the number of balancers, orchestrators and uptime checks watching it —
// each uncached probe spends a pooled DB connection.
func TestReadyzCachesTheProbe(t *testing.T) {
	db, rdb := &countingPinger{}, &countingPinger{}
	srv := newHealthServer(db, rdb)

	for i := 0; i < 5; i++ {
		if code, _ := getReadyz(t, srv); code != http.StatusOK {
			t.Fatalf("call %d: status = %d, want 200", i, code)
		}
	}
	if got := db.calls(); got != 1 {
		t.Errorf("postgres pinged %d times for 5 readiness calls, want 1", got)
	}
	if got := rdb.calls(); got != 1 {
		t.Errorf("redis pinged %d times for 5 readiness calls, want 1", got)
	}

	// Aging the cache out re-probes.
	srv.readinessMu.Lock()
	srv.readinessCached.at = time.Now().Add(-2 * readinessCacheTTL)
	srv.readinessMu.Unlock()
	if code, _ := getReadyz(t, srv); code != http.StatusOK {
		t.Fatal("status after cache expiry != 200")
	}
	if got := db.calls(); got != 2 {
		t.Errorf("postgres pinged %d times after the cache aged out, want 2", got)
	}
}
