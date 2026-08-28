package api

// TWIN: vidra-core internal/httpapi/health.go — keep readiness semantics in sync.

import (
	"context"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/vidra/vidra-search/internal/version"
)

type livenessResponse struct {
	Status string `json:"status"`
}

type componentStatus struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// readinessResponse is returned by GET /readyz.
//
// Status is one of:
//
//	ok          — everything this instance needs is reachable (HTTP 200)
//	degraded    — a NON-CRITICAL dependency is down; the instance is still
//	              serving, and a load balancer must keep routing to it (200)
//	unavailable — PostgreSQL is unreachable; nothing this service does works (503)
//	draining    — shutdown has begun and the listener is about to close; stop
//	              routing here (503)
//
// The HTTP status code is the load balancer's contract and the body is the
// operator's.
type readinessResponse struct {
	Status     string                     `json:"status"`
	Components map[string]componentStatus `json:"components"`
}

// readinessSnapshot is one cached probe result: the body and the code that go
// with it, plus when it was taken.
type readinessSnapshot struct {
	resp readinessResponse
	code int
	at   time.Time
}

// readinessCacheTTL is how long one probe result is reused. Short enough that a
// dependency outage is visible within one balancer health-check interval, long
// enough that N watchers cost the same as one.
const readinessCacheTTL = 2 * time.Second

// handleLive reports the process is up and serving. It performs no dependency
// checks so an orchestrator can distinguish "process alive" from "ready".
func (s *Server) handleLive(c echo.Context) error {
	return c.JSON(http.StatusOK, livenessResponse{Status: "ok"})
}

// handleReady answers the load balancer's question — "should I send this
// instance traffic?" — which is deliberately NOT the same question as "is
// everything about this instance fine".
//
// Only PostgreSQL takes an instance out of rotation. It is where every
// document, aggregate and event lives; a replica that cannot reach it has
// nothing to serve. Redis does not: it backs the suggestion/search caches and
// the event-dedupe set, all of which the services fall back from on error, so a
// Redis blip is a degradation. 503ing on it would take EVERY replica out of
// rotation SIMULTANEOUSLY — they all share the same Redis — turning a lost
// cache into a total outage. It is reported as a degraded component with a 200.
//
// Draining wins over everything: once Drain() has been called this instance is
// leaving, and it says so before the listener closes so the balancer has a
// chance to notice.
func (s *Server) handleReady(c echo.Context) error {
	if s.draining.Load() {
		// Not cached and not probed: the answer does not depend on any
		// dependency, and spending a pooled connection to decorate a 503 nobody
		// will route on would be the opposite of what draining is for.
		return c.JSON(http.StatusServiceUnavailable, readinessResponse{
			Status:     "draining",
			Components: map[string]componentStatus{},
		})
	}
	snap := s.readiness(c.Request().Context())
	return c.JSON(snap.code, snap.resp)
}

// readiness returns the probe result, taking a fresh one only when the cached
// one has aged out. Concurrent callers wait on the same lock and share the
// result rather than each opening their own probe: readiness is polled by
// everything in front of the instance, and the cost of answering it must not
// scale with how many things are watching.
func (s *Server) readiness(ctx context.Context) readinessSnapshot {
	now := time.Now()

	s.readinessMu.Lock()
	defer s.readinessMu.Unlock()
	if cached := s.readinessCached; cached != nil && now.Sub(cached.at) < readinessCacheTTL {
		return *cached
	}

	components, _ := s.componentHealth(ctx)
	snap := readinessSnapshot{
		resp: readinessResponse{Status: "ok", Components: components},
		code: http.StatusOK,
		at:   now,
	}
	switch {
	case components["postgres"].Status == "down":
		snap.resp.Status = "unavailable"
		snap.code = http.StatusServiceUnavailable
	case anyComponentDown(components):
		// Reachable database, something else down. Still serving.
		snap.resp.Status = "degraded"
	}
	s.readinessCached = &snap
	return snap
}

// anyComponentDown reports whether any probed dependency answered "down".
// "not_configured" is not a fault — a Server constructed without a Redis is a
// supported shape (the contract test does exactly that), not a broken one.
func anyComponentDown(components map[string]componentStatus) bool {
	for _, c := range components {
		if c.Status == "down" {
			return true
		}
	}
	return false
}

func (s *Server) componentHealth(ctx context.Context) (map[string]componentStatus, bool) {
	components := map[string]componentStatus{}
	healthy := true
	check := func(name string, p Pinger) {
		if p == nil {
			components[name] = componentStatus{Status: "not_configured"}
			return
		}
		if err := p.Ping(ctx); err != nil {
			healthy = false
			components[name] = componentStatus{Status: "down", Error: err.Error()}
			return
		}
		components[name] = componentStatus{Status: "ok"}
	}
	check("postgres", s.db)
	check("redis", s.rdb)
	return components, healthy
}

type versionResponse struct {
	Name      string `json:"name"`
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildDate string `json:"build_date"`
	Go        string `json:"go"`
}

// handleVersion reports the running build's metadata. It exposes only build
// information, never secrets or configuration.
func (s *Server) handleVersion(c echo.Context) error {
	return c.JSON(http.StatusOK, versionResponse{
		Name:      "vidra-search",
		Version:   version.Version,
		Commit:    version.Commit,
		BuildDate: version.Date,
		Go:        version.GoVersion(),
	})
}
