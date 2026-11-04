package event

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vidra/vidra-search/internal/catalog"
	"github.com/vidra/vidra-search/internal/store"
)

// IngestCatalog applies only v2 full observations. Domain identity/content is
// revalidated on every replay; the expiring legacy inbox is never consulted.
// HTTP framing/size validation remains the transport's responsibility.
func (s *Service) IngestCatalog(ctx context.Context, events []Envelope) (Result, error) {
	res := Result{Failed: []Failure{}}
	if len(events) == 0 || len(events) > MaxBatch {
		return res, fmt.Errorf("catalog: batch requires 1..%d events", MaxBatch)
	}
	tx, err := s.store.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return res, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := store.LockCatalog(ctx, tx); err != nil {
		return res, err
	}
	for _, ev := range events {
		if err := applyCatalogOne(ctx, tx, ev); err != nil {
			code := "catalog_invalid"
			switch {
			case errors.Is(err, store.ErrCatalogAlreadyCompleted):
				code = "catalog_already_completed"
			case errors.Is(err, store.ErrCatalogQuota):
				code = "catalog_quota"
			case errors.Is(err, store.ErrCatalogConflict):
				code = "catalog_conflict"
			}
			res.Failed = append(res.Failed, Failure{EventID: ev.EventID.String(), Code: code, Message: err.Error()})
			continue
		}
		res.Accepted++ // Staged replay may be accepted; this is not completion proof.
	}
	if err := tx.Commit(ctx); err != nil {
		return Result{Failed: []Failure{}}, err
	}
	return res, nil
}

func applyCatalogOne(ctx context.Context, tx pgx.Tx, ev Envelope) error {
	if ev.EventID == uuid.Nil || ev.SchemaVersion != 2 || ev.OccurredAt.IsZero() {
		return fmt.Errorf("catalog: invalid envelope")
	}
	sub, err := tx.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = sub.Rollback(ctx) }()
	var identity catalog.Identity
	switch ev.Type {
	case catalog.TypeSnapshotPage:
		page, err := parseCatalogPage(ev.Payload)
		if err != nil {
			return err
		}
		identity = page.Identity
		if err := store.StageCatalogPage(ctx, sub, identity, page.PageDigest, page.CanonicalDocuments); err != nil {
			return err
		}
	case catalog.TypeSnapshotEnd:
		end, err := parseCatalogEnd(ev.Payload)
		if err != nil {
			return err
		}
		identity = end.Identity
		if err := store.StageCatalogEnd(ctx, sub, identity, end.Manifest); err != nil {
			return err
		}
	default:
		return fmt.Errorf("catalog: unsupported event type")
	}
	if _, err := store.CompleteCatalogSnapshot(ctx, sub, identity); err != nil {
		return err
	}
	return sub.Commit(ctx)
}
