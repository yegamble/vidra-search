package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"

	"github.com/vidra/vidra-search/internal/catalog"
	"github.com/vidra/vidra-search/internal/event"
	"github.com/vidra/vidra-search/internal/store"
)

func readCatalogBody(c *echo.Context) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(c.Request().Body, catalog.MaxRequestBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > catalog.MaxRequestBytes {
		return nil, echo.NewHTTPError(http.StatusRequestEntityTooLarge, "catalog request exceeds byte budget")
	}
	return raw, nil
}

func (s *Server) handleCatalogStatus(c *echo.Context) error {
	scopes := c.QueryParams()["scope"]
	if len(scopes) != 1 || scopes[0] != "local" {
		return newValidation("scope", "one supported local scope is required")
	}
	reader, ok := s.db.(catalogStatusReader)
	if !ok {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "catalog authority unavailable")
	}
	status, err := reader.ReadCatalogStatus(c.Request().Context(), "local")
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, status)
}

type catalogEpochSetter interface {
	SetCatalogEpoch(context.Context, string, uuid.UUID, *uuid.UUID) error
}

func (s *Server) handleCatalogEpoch(c *echo.Context) error {
	raw, err := readCatalogBody(c)
	if err != nil {
		return err
	}
	fields, err := catalog.ObjectFields(raw, []string{"scope", "epoch", "expected_epoch"}, []string{"expected_epoch"})
	if err != nil {
		return newValidation("catalog_epoch", "invalid exact epoch request")
	}
	var request struct {
		Scope         string     `json:"scope"`
		Epoch         uuid.UUID  `json:"epoch"`
		ExpectedEpoch *uuid.UUID `json:"expected_epoch"`
	}
	if err := json.Unmarshal(raw, &request); err != nil || request.Scope != "local" {
		return newValidation("catalog_epoch", "invalid local epoch request")
	}
	for key, id := range map[string]*uuid.UUID{"epoch": &request.Epoch, "expected_epoch": request.ExpectedEpoch} {
		if id == nil {
			continue
		}
		var text string
		if *id == uuid.Nil || json.Unmarshal(fields[key], &text) != nil || text != id.String() {
			return newValidation("catalog_epoch", "noncanonical epoch identity")
		}
	}
	setter, ok := s.db.(catalogEpochSetter)
	if !ok {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "catalog authority unavailable")
	}
	if err := setter.SetCatalogEpoch(c.Request().Context(), request.Scope, request.Epoch, request.ExpectedEpoch); err != nil {
		if errors.Is(err, store.ErrCatalogConflict) {
			return echo.NewHTTPError(http.StatusConflict, "catalog epoch conflict")
		}
		return err
	}
	// A separate status read is not part of the CAS. The producer must verify it.
	return c.NoContent(http.StatusNoContent)
}

func (s *Server) handleCatalogEvents(c *echo.Context) error {
	raw, err := readCatalogBody(c)
	if err != nil {
		return err
	}
	events, err := event.DecodeCatalogBatch(raw)
	if err != nil {
		return newValidation("events", "invalid exact catalog event batch")
	}
	result, err := s.svcs.Events.IngestCatalog(c.Request().Context(), events)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, result)
}
