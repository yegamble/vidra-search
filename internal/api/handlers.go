package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/vidra/vidra-search/internal/event"
	"github.com/vidra/vidra-search/internal/search"
	"github.com/vidra/vidra-search/internal/suggest"
)

// handleSuggestions serves GET /internal/v1/suggestions. It always returns 200:
// the pipeline degrades to an empty list on any internal trouble.
func (s *Server) handleSuggestions(c echo.Context) error {
	req := suggest.Request{
		Query:          c.QueryParam("q"),
		Limit:          qInt(c, "limit"),
		HideSensitive:  qBool(c, "hide_sensitive"),
		Personalized:   qBool(c, "personalized"),
		IncludeHistory: qBool(c, "include_history"),
		Lang:           c.QueryParam("lang"),
		UserID:         c.QueryParam("user_id"),
		SessionID:      c.QueryParam("session_id"),
		Mode:           c.QueryParam("mode"),
	}
	start := time.Now()
	resp := s.svcs.Suggest.Suggest(c.Request().Context(), req)
	if s.metrics != nil {
		// Observe the suggestion pipeline in isolation (separate from the generic
		// HTTP request timer), matching the vidra_search_suggest_duration_seconds
		// spec metric.
		s.metrics.ObserveSuggest(time.Since(start))
	}
	return c.JSON(http.StatusOK, resp)
}

// handleSearch serves GET /internal/v1/search — simple-mode hybrid search.
func (s *Server) handleSearch(c echo.Context) error {
	req := search.Request{
		Query:         c.QueryParam("q"),
		Limit:         qInt(c, "limit"),
		Offset:        qInt(c, "offset"),
		Tag:           c.QueryParam("tag"),
		Category:      c.QueryParam("category"),
		Language:      c.QueryParam("language"),
		HideSensitive: qBool(c, "hide_sensitive"),
		Mode:          c.QueryParam("mode"),
	}
	resp, err := s.svcs.Search.Search(c.Request().Context(), req)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, resp)
}

// handleRelated serves GET /internal/v1/recommendations/related?video_id=...
func (s *Server) handleRelated(c echo.Context) error {
	videoID, err := uuid.Parse(c.QueryParam("video_id"))
	if err != nil {
		return newValidation("video_id", "must be a valid UUID")
	}
	resp, err := s.svcs.Rec.Related(c.Request().Context(), videoID, qInt(c, "limit"), qBool(c, "hide_sensitive"))
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, resp)
}

// handleHome serves GET /internal/v1/recommendations/home.
func (s *Server) handleHome(c echo.Context) error {
	resp, err := s.svcs.Rec.Home(c.Request().Context(), qInt(c, "limit"), qBool(c, "hide_sensitive"), c.QueryParam("lang"))
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, resp)
}

// eventsRequest is the POST /internal/v1/events body.
type eventsRequest struct {
	Events []event.Envelope `json:"events"`
}

// handleEvents serves POST /internal/v1/events: dedupe + apply a batch (≤500).
func (s *Server) handleEvents(c echo.Context) error {
	var body eventsRequest
	if err := c.Bind(&body); err != nil {
		return newValidation("events", "invalid JSON body")
	}
	if len(body.Events) == 0 {
		return newValidation("events", "at least one event is required")
	}
	if len(body.Events) > event.MaxBatch {
		return newValidation("events", "at most "+strconv.Itoa(event.MaxBatch)+" events per batch")
	}
	res, err := s.svcs.Events.Ingest(c.Request().Context(), body.Events)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, res)
}

// --- query param helpers ---

// qInt returns the integer query value, or 0 when absent/invalid (the service
// layers then apply their own default + clamp).
func qInt(c echo.Context, name string) int {
	v := c.QueryParam(name)
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0
	}
	return n
}

// qBool returns the boolean query value (true only for a truthy value).
func qBool(c echo.Context, name string) bool {
	b, err := strconv.ParseBool(c.QueryParam(name))
	if err != nil {
		return false
	}
	return b
}
