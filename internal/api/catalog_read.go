package api

import (
	"context"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/vidra/vidra-search/internal/catalog"
)

type catalogStatusReader interface {
	ReadCatalogStatus(context.Context, string) (catalog.Status, error)
}

type catalogReadFence struct{ epoch, generation string }

func (s *Server) beginCatalogRead(c echo.Context) (catalogReadFence, error) {
	unavailable := func() error { return echo.NewHTTPError(http.StatusServiceUnavailable, "catalog authority unavailable") }
	reader, ok := s.db.(catalogStatusReader)
	if !ok {
		return catalogReadFence{}, unavailable()
	}
	status, err := reader.ReadCatalogStatus(c.Request().Context(), "local")
	if err != nil {
		return catalogReadFence{}, echo.NewHTTPError(http.StatusServiceUnavailable, "catalog authority unavailable").SetInternal(err)
	}
	if status.Epoch == nil {
		return catalogReadFence{}, nil // Legacy reads remain available until explicit initialization.
	}
	fence := catalogReadFence{status.Epoch.String(), status.CompletedGeneration}
	if !status.Ready || fence.generation == "0" || c.Request().Header.Get("X-Vidra-Catalog-Epoch") != fence.epoch ||
		c.Request().Header.Get("X-Vidra-Catalog-Generation") != fence.generation {
		return catalogReadFence{}, unavailable()
	}
	return fence, nil
}

// Check again before committing success. No DB lock/connection spans service
// queries; a newer full or epoch must reject a response computed across fences.
func (s *Server) finishCatalogRead(c echo.Context, before catalogReadFence) error {
	after, err := s.beginCatalogRead(c)
	if err != nil {
		return err
	}
	if before != after {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "catalog authority changed")
	}
	return nil
}
