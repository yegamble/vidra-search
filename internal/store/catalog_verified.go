package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vidra/vidra-search/internal/catalog"
)

// VerifiedCatalogSnapshot is pending evidence only, not completed authority.
type VerifiedCatalogSnapshot struct {
	manifest catalog.Manifest
}

// LoadVerifiedCatalogSnapshot reads current pending evidence under the catalog
// lock. Nil means incomplete or already completed. This never changes authority.
// Keep the caller's READ COMMITTED transaction open through any later activation.
func LoadVerifiedCatalogSnapshot(ctx context.Context, tx pgx.Tx, id catalog.Identity) (*VerifiedCatalogSnapshot, error) {
	if err := id.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCatalogConflict, err)
	}
	if err := LockCatalog(ctx, tx); err != nil {
		return nil, err
	}
	var epoch uuid.UUID
	var highest, completed int64
	err := tx.QueryRow(ctx, `SELECT epoch, highest_observed_generation, completed_generation
		FROM search.catalog_scopes WHERE scope=$1 FOR UPDATE`, id.Scope).Scan(&epoch, &highest, &completed)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: uninitialized scope", ErrCatalogConflict)
	}
	if err != nil {
		return nil, err
	}
	if epoch != id.Epoch {
		return nil, fmt.Errorf("%w: epoch mismatch", ErrCatalogConflict)
	}
	if int64(id.Generation) == completed {
		return nil, nil
	}
	if int64(id.Generation) != highest {
		return nil, fmt.Errorf("%w: observation is not current", ErrCatalogConflict)
	}
	var pageCount *int32
	var documentCount *int64
	var membership, content *string
	var charged int64
	var fresh bool
	err = tx.QueryRow(ctx, `SELECT page_count, document_count, membership_sha256, pages_sha256,
		canonical_bytes, created_at > clock_timestamp()-interval '24 hours'
		FROM search.catalog_snapshots WHERE scope=$1 AND epoch=$2 AND generation=$3 FOR UPDATE`,
		id.Scope, id.Epoch, int64(id.Generation)).Scan(&pageCount, &documentCount, &membership, &content, &charged, &fresh)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !fresh {
		return nil, fmt.Errorf("%w: expired pending observation", ErrCatalogConflict)
	}
	if err != nil {
		return nil, err
	}
	if pageCount == nil {
		return nil, nil
	}
	snapshot := &VerifiedCatalogSnapshot{manifest: catalog.Manifest{PageCount: *pageCount, DocumentCount: *documentCount,
		MembershipSHA256: *membership, PagesSHA256: *content}}
	rows, err := tx.Query(ctx, `SELECT page_index, documents, document_count, content_sha256
		FROM search.catalog_snapshot_pages WHERE scope=$1 AND epoch=$2 AND generation=$3
		ORDER BY page_index FOR UPDATE`, id.Scope, id.Epoch, int64(id.Generation))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var pages []catalog.SnapshotPage
	for rows.Next() {
		var page catalog.SnapshotPage
		if err := rows.Scan(&page.PageIndex, &page.Documents, &page.DocumentCount, &page.ContentSHA256); err != nil {
			return nil, err
		}
		pages = append(pages, page)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(pages) < int(snapshot.manifest.PageCount) {
		return nil, nil
	}
	if err := catalog.VerifySnapshot(snapshot.manifest, pages, charged); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCatalogConflict, err)
	}
	return snapshot, nil
}
