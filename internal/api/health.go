package api

import (
	"context"
	"net/http"

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

type readinessResponse struct {
	Status     string                     `json:"status"`
	Components map[string]componentStatus `json:"components"`
}

// handleLive reports the process is up and serving. It performs no dependency
// checks so an orchestrator can distinguish "process alive" from "ready".
func (s *Server) handleLive(c echo.Context) error {
	return c.JSON(http.StatusOK, livenessResponse{Status: "ok"})
}

// handleReady reports whether all critical dependencies (postgres, redis) are
// reachable. Returns 503 with per-component detail if any is unhealthy.
func (s *Server) handleReady(c echo.Context) error {
	components, ready := s.componentHealth(c.Request().Context())
	resp := readinessResponse{Components: components}
	if ready {
		resp.Status = "ok"
		return c.JSON(http.StatusOK, resp)
	}
	resp.Status = "degraded"
	return c.JSON(http.StatusServiceUnavailable, resp)
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
