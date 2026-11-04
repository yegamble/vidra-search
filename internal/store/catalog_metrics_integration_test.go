//go:build integration

package store_test

import (
	"context"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/vidra/vidra-search/internal/event"
)

type catalogIntakeMetrics struct {
	events []string
	lags   int
}

func (m *catalogIntakeMetrics) ObserveEvent(typ, outcome string) {
	m.events = append(m.events, typ+":"+outcome)
}
func (m *catalogIntakeMetrics) ObserveEventLag(float64) { m.lags++ }

func TestCatalogEventMetricsIncludeStagedReplaysAndBoundedFailures(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	epoch := uuid.New()
	if err := env.store.SetCatalogEpoch(ctx, "local", epoch, nil); err != nil {
		t.Fatal(err)
	}
	metrics := &catalogIntakeMetrics{}
	svc := event.NewService(env.store, metrics, nil, event.Config{}, nil)
	batch := catalogFullEvents(t, epoch, 1, video("metrics full"))
	unknown := batch[0]
	unknown.Type = "unbounded-attacker-value"
	if _, err := svc.IngestCatalog(ctx, append(append([]event.Envelope(nil), batch...), unknown)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.IngestCatalog(ctx, batch); err != nil {
		t.Fatal(err)
	}
	want := []string{"catalog.snapshot.page:accepted", "catalog.snapshot.end:accepted", "unknown:failed",
		"catalog.snapshot.page:failed", "catalog.snapshot.end:accepted"}
	if !slices.Equal(metrics.events, want) || metrics.lags != 3 {
		t.Fatalf("missing/bad catalog intake metrics: events=%v lags=%d", metrics.events, metrics.lags)
	}
}
