package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/labstack/echo/v5"

	"github.com/vidra/vidra-search/internal/catalog"
)

type catalogStatusReader interface {
	ReadCatalogStatus(context.Context, string) (catalog.Status, error)
}

type catalogReadFence struct{ epoch, generation string }

func (s *Server) beginCatalogRead(c *echo.Context) (catalogReadFence, error) {
	unavailable := func() error { return echo.NewHTTPError(http.StatusServiceUnavailable, "catalog authority unavailable") }
	reader, ok := s.db.(catalogStatusReader)
	if !ok {
		return catalogReadFence{}, unavailable()
	}
	status, err := reader.ReadCatalogStatus(c.Request().Context(), "local")
	if err != nil {
		return catalogReadFence{}, echo.NewHTTPError(http.StatusServiceUnavailable, "catalog authority unavailable").Wrap(err)
	}
	if status.Epoch == nil {
		// A declared fence requires authority even before initialization or after
		// restore. Header.Get cannot distinguish an absent header from an empty one.
		for name := range c.Request().Header {
			if strings.EqualFold(name, "X-Vidra-Catalog-Epoch") || strings.EqualFold(name, "X-Vidra-Catalog-Generation") {
				return catalogReadFence{}, unavailable()
			}
		}
		return catalogReadFence{}, nil // Wholly unfenced legacy reads remain available.
	}
	fence := catalogReadFence{status.Epoch.String(), status.CompletedGeneration}
	if !status.Ready || status.ForeignEligible || fence.generation == "0" || c.Request().Header.Get("X-Vidra-Catalog-Epoch") != fence.epoch ||
		c.Request().Header.Get("X-Vidra-Catalog-Generation") != fence.generation {
		return catalogReadFence{}, unavailable()
	}
	return fence, nil
}

// Check again before committing success. No DB lock/connection spans service
// queries; a newer full or epoch must reject a response computed across fences.
func (s *Server) finishCatalogRead(c *echo.Context, before catalogReadFence) error {
	after, err := s.beginCatalogRead(c)
	if err != nil {
		return err
	}
	if before != after {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "catalog authority changed")
	}
	return nil
}
