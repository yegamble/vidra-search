package event

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/vidra/vidra-search/internal/store"
)

func isLegacyCatalogType(typ string) bool {
	switch typ {
	case TypeVideoUpsert, TypeVideoSuppress, TypeChannelUpsert, TypeChannelDelete,
		TypeUserSuppress, TypeReconcileBegin, TypeReconcilePage, TypeReconcileEnd:
		return true
	}
	return false
}

func legacyCatalogBlocked(ctx context.Context, tx pgx.Tx, events []Envelope) (bool, error) {
	for _, ev := range events {
		if !isLegacyCatalogType(ev.Type) {
			continue
		}
		// Serialize before checking authority, then hold through commit. A writer
		// admitted before bootstrap finishes first; one waiting behind it fails.
		if err := store.LockCatalog(ctx, tx); err != nil {
			return false, err
		}
		var initialized bool
		err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM search.catalog_scopes
			WHERE scope='local')`).Scan(&initialized)
		return initialized, err
	}
	return false, nil
}
