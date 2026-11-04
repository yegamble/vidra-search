package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// PruneCatalogSnapshots deletes expired pending payloads, never epoch history or
// admission/completion fences. A late replay cannot recreate an expired highest
// generation. Use the same authority lock as admission, activation and reset.
func (s *Store) PruneCatalogSnapshots(ctx context.Context) (int64, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := LockCatalog(ctx, tx); err != nil {
		return 0, err
	}
	deleted, err := tx.Exec(ctx, `DELETE FROM search.catalog_snapshots
		WHERE created_at <= clock_timestamp()-interval '24 hours'`)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return deleted.RowsAffected(), nil
}
