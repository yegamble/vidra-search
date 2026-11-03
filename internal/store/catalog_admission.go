package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vidra/vidra-search/internal/catalog"
)

var ErrCatalogAlreadyCompleted = errors.New("catalog generation already completed")

// AdmitCatalogGeneration admits a validated observation into the newest pending
// slot. The caller owns a READ COMMITTED transaction and must roll it back when
// subsequent page/manifest validation fails. No live projection is changed here.
func AdmitCatalogGeneration(ctx context.Context, tx pgx.Tx, identity catalog.Identity) error {
	if err := identity.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrCatalogConflict, err)
	}
	if err := LockCatalog(ctx, tx); err != nil {
		return err
	}
	var epoch uuid.UUID
	var highest, completed int64
	err := tx.QueryRow(ctx, `SELECT epoch, highest_observed_generation, completed_generation
		FROM search.catalog_scopes WHERE scope=$1`, identity.Scope).Scan(&epoch, &highest, &completed)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: uninitialized scope", ErrCatalogConflict)
	}
	if err != nil {
		return err
	}
	if epoch != identity.Epoch {
		return fmt.Errorf("%w: epoch mismatch", ErrCatalogConflict)
	}
	generation := int64(identity.Generation)
	if generation == completed {
		// Completed payloads may have been pruned. Only the durable full manifest
		// can establish an idempotent completion, never a replayed page receipt.
		return ErrCatalogAlreadyCompleted
	}
	if generation < highest {
		return fmt.Errorf("%w: superseded generation", ErrCatalogConflict)
	}
	if generation == highest {
		var pending bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM search.catalog_snapshots
			WHERE scope=$1 AND epoch=$2 AND generation=$3)`, identity.Scope, epoch, generation).Scan(&pending); err != nil {
			return err
		}
		if !pending {
			return fmt.Errorf("%w: expired generation requires a fresh observation", ErrCatalogConflict)
		}
		return nil
	}
	if _, err := tx.Exec(ctx, `DELETE FROM search.catalog_snapshots WHERE scope=$1`, identity.Scope); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE search.catalog_scopes SET highest_observed_generation=$2
		WHERE scope=$1`, identity.Scope, generation); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO search.catalog_snapshots(scope, epoch, generation)
		VALUES ($1, $2, $3)`, identity.Scope, epoch, generation)
	return err
}
