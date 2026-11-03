package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vidra/vidra-search/internal/catalog"
)

var ErrCatalogConflict = errors.New("catalog authority conflict")

// LockCatalog serializes initialization, activation and legacy write admission.
// It is transaction-scoped so a pre-initialization writer cannot commit after
// an epoch becomes authoritative. Callers must not hold it across network I/O.
// Use READ COMMITTED so queries after waiting observe the lock holder's commit.
func LockCatalog(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(0x7669647261636174))
	return err
}

// SetCatalogEpoch explicitly initializes or resets local observation authority.
// Epoch history is permanent; only an exact retry with the original predecessor
// is idempotent. No HTTP route calls this foundation until rollout is complete.
func (s *Store) SetCatalogEpoch(ctx context.Context, scope string, epoch uuid.UUID, expected *uuid.UUID) error {
	if scope != catalog.ScopeLocal || epoch == uuid.Nil || expected != nil && *expected == uuid.Nil {
		return fmt.Errorf("catalog epoch requires local scope and nonzero identities")
	}
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := LockCatalog(ctx, tx); err != nil {
		return err
	}
	var current uuid.UUID
	err = tx.QueryRow(ctx, `SELECT epoch FROM search.catalog_scopes WHERE scope=$1`, scope).Scan(&current)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if err == nil && current == epoch {
		var predecessor pgtype.UUID
		if err := tx.QueryRow(ctx, `SELECT predecessor FROM search.catalog_epochs
			WHERE scope=$1 AND epoch=$2`, scope, epoch).Scan(&predecessor); err != nil {
			return err
		}
		if predecessor.Valid != (expected != nil) || expected != nil && uuid.UUID(predecessor.Bytes) != *expected {
			return ErrCatalogConflict
		}
		return tx.Commit(ctx)
	}
	if err == nil && (expected == nil || *expected != current) || errors.Is(err, pgx.ErrNoRows) && expected != nil {
		return ErrCatalogConflict
	}
	var used bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM search.catalog_epochs
		WHERE scope=$1 AND epoch=$2)`, scope, epoch).Scan(&used); err != nil {
		return err
	}
	if used {
		return ErrCatalogConflict
	}
	if _, err := tx.Exec(ctx, `INSERT INTO search.catalog_epochs(scope, epoch, predecessor)
		VALUES ($1, $2, $3)`, scope, epoch, expected); err != nil {
		return err
	}
	// Remove the old epoch's pending payloads before changing its referenced key.
	// Live documents survive reset but cannot be served until a fresh full exists.
	if _, err := tx.Exec(ctx, `DELETE FROM search.catalog_snapshots WHERE scope=$1`, scope); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO search.catalog_scopes(scope, epoch) VALUES ($1, $2)
		ON CONFLICT (scope) DO UPDATE SET epoch=EXCLUDED.epoch,
		highest_observed_generation=0, completed_generation=0, completed_at=NULL,
		completed_page_count=0, completed_document_count=0,
		completed_membership_sha256=NULL, completed_pages_sha256=NULL`, scope, epoch); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
