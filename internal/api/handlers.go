package api

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/vidra/vidra-search/internal/event"
	"github.com/vidra/vidra-search/internal/moderation"
	"github.com/vidra/vidra-search/internal/recommendation"
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
		License:       c.QueryParam("license"),
		HideSensitive: qBool(c, "hide_sensitive"),
		Mode:          c.QueryParam("mode"),
		UserID:        c.QueryParam("user_id"),
		SessionID:     c.QueryParam("session_id"),
		Personalized:  qBool(c, "personalized"),
		// Absent/invalid skip_count parses false, i.e. the total IS computed.
		SkipCount: qBool(c, "skip_count"),
	}
	resp, err := s.svcs.Search.Search(c.Request().Context(), req)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, resp)
}

// handleRelated serves GET /internal/v1/recommendations/related?video_id=...
func (s *Server) handleRelated(c echo.Context) error {
	videoID, err := queryUUID(c, "video_id")
	if err != nil {
		return err
	}
	resp, err := s.svcs.Rec.Related(c.Request().Context(), recommendation.RelatedRequest{
		VideoID:       videoID,
		Limit:         qInt(c, "limit"),
		HideSensitive: qBool(c, "hide_sensitive"),
		UserID:        c.QueryParam("user_id"),
		SessionID:     c.QueryParam("session_id"),
		Personalized:  qBool(c, "personalized"),
	})
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, resp)
}

// handleHome serves GET /internal/v1/recommendations/home.
func (s *Server) handleHome(c echo.Context) error {
	resp, err := s.svcs.Rec.Home(c.Request().Context(), recommendation.HomeRequest{
		Limit:         qInt(c, "limit"),
		HideSensitive: qBool(c, "hide_sensitive"),
		Lang:          c.QueryParam("lang"),
		UserID:        c.QueryParam("user_id"),
		SessionID:     c.QueryParam("session_id"),
		Personalized:  qBool(c, "personalized"),
	})
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

// handleGetSearchHistory serves GET /internal/v1/users/{user_id}/search-history.
func (s *Server) handleGetSearchHistory(c echo.Context) error {
	userID, err := pathUUID(c, "user_id")
	if err != nil {
		return err
	}
	resp, err := s.svcs.History.List(c.Request().Context(), userID, qInt(c, "limit"), qInt(c, "offset"))
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, resp)
}

// handleClearSearchHistory serves DELETE /internal/v1/users/{user_id}/search-history:
// clears the user's history and anonymizes their raw logs.
func (s *Server) handleClearSearchHistory(c echo.Context) error {
	userID, err := pathUUID(c, "user_id")
	if err != nil {
		return err
	}
	if err := s.svcs.History.ClearAll(c.Request().Context(), userID); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}

// handleDeleteSearchHistoryEntry serves
// DELETE /internal/v1/users/{user_id}/search-history/{normalized_query}. Echo has
// already URL-decoded the path param, so it is used directly as the normalized
// query key.
func (s *Server) handleDeleteSearchHistoryEntry(c echo.Context) error {
	userID, err := pathUUID(c, "user_id")
	if err != nil {
		return err
	}
	normalizedQuery := c.Param("normalized_query")
	if err := s.svcs.History.DeleteEntry(c.Request().Context(), userID, normalizedQuery); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}

// handleDeleteUser serves DELETE /internal/v1/users/{user_id}: a full privacy
// purge (history, projections, anonymized logs).
func (s *Server) handleDeleteUser(c echo.Context) error {
	userID, err := pathUUID(c, "user_id")
	if err != nil {
		return err
	}
	if err := s.svcs.History.PurgeUser(c.Request().Context(), userID); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}

// handleListSuggestionBans serves GET /internal/v1/suggestions/bans: the paged
// list of queries currently suppressed from instance-wide autosuggest, so a ban
// can be reviewed and reversed by someone who did not place it.
func (s *Server) handleListSuggestionBans(c echo.Context) error {
	resp, err := s.svcs.Moderation.List(c.Request().Context(), qInt(c, "limit"), qInt(c, "offset"))
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, resp)
}

// handleBanSuggestion serves PUT /internal/v1/suggestions/bans/{normalized_query}.
// Echo has already URL-decoded the path param; the service normalizes it, and the
// response echoes the key that actually moved so a later unban can target it.
// PUT, not POST: banning the same query twice is the same end state, not a second
// ban.
func (s *Server) handleBanSuggestion(c echo.Context) error {
	resp, err := s.svcs.Moderation.Ban(c.Request().Context(), c.Param("normalized_query"))
	if err != nil {
		return moderationError(err)
	}
	return c.JSON(http.StatusOK, resp)
}

// handleUnbanSuggestion serves DELETE /internal/v1/suggestions/bans/{normalized_query}.
// Idempotent: unbanning a query that is not banned is still a 204.
func (s *Server) handleUnbanSuggestion(c echo.Context) error {
	if err := s.svcs.Moderation.Unban(c.Request().Context(), c.Param("normalized_query")); err != nil {
		return moderationError(err)
	}
	return c.NoContent(http.StatusNoContent)
}

// moderationError maps the one client-caused moderation failure to the standard
// 422 envelope; everything else stays a 500.
func moderationError(err error) error {
	var empty moderation.ErrEmptyQuery
	if errors.As(err, &empty) {
		return newValidation("normalized_query", "must not be empty after normalization")
	}
	return err
}

// --- param helpers ---

// pathUUID parses a UUID path parameter, or returns the 422 validation error
// naming the offending parameter. Every id in this API is a UUID, so the
// parse-or-422 is the same four lines at every route; having one copy is what
// keeps the field name in the error body consistent with the route template.
func pathUUID(c echo.Context, name string) (uuid.UUID, error) {
	id, err := uuid.Parse(c.Param(name))
	if err != nil {
		return uuid.Nil, newValidation(name, "must be a valid UUID")
	}
	return id, nil
}

// queryUUID is pathUUID for a required query parameter. Absent and malformed
// are the same answer: a caller who did not name a video cannot be served one.
func queryUUID(c echo.Context, name string) (uuid.UUID, error) {
	id, err := uuid.Parse(c.QueryParam(name))
	if err != nil {
		return uuid.Nil, newValidation(name, "must be a valid UUID")
	}
	return id, nil
}

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
