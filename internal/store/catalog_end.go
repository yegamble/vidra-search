package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/vidra/vidra-search/internal/catalog"
)

// StageCatalogEnd records an immutable manifest, never completion. The caller
// owns a READ COMMITTED transaction and rolls back failed event admission.
func StageCatalogEnd(ctx context.Context, tx pgx.Tx, identity catalog.Identity, manifest catalog.Manifest) error {
	if err := manifest.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrCatalogConflict, err)
	}
	if manifest.PageCount > catalog.MaxPages || manifest.DocumentCount > catalog.MaxDocuments {
		return fmt.Errorf("%w: manifest counts", ErrCatalogQuota)
	}
	admission := AdmitCatalogGeneration(ctx, tx, identity)
	if errors.Is(admission, ErrCatalogAlreadyCompleted) {
		var completed catalog.Manifest
		if err := tx.QueryRow(ctx, `SELECT completed_page_count, completed_document_count,
			completed_membership_sha256, completed_pages_sha256 FROM search.catalog_scopes
			WHERE scope=$1 AND epoch=$2 AND completed_generation=$3`, identity.Scope,
			identity.Epoch, int64(identity.Generation)).Scan(&completed.PageCount, &completed.DocumentCount,
			&completed.MembershipSHA256, &completed.PagesSHA256); err != nil {
			return err
		}
		if completed != manifest {
			return fmt.Errorf("%w: completed manifest mismatch", ErrCatalogConflict)
		}
		return nil // Exact end replay does not touch a newer pending generation.
	}
	if admission != nil {
		return admission
	}
	var expectedPages *int32
	var expectedCount *int64
	var membership, pagesDigest *string
	if err := tx.QueryRow(ctx, `SELECT page_count, document_count, membership_sha256, pages_sha256
		FROM search.catalog_snapshots WHERE scope=$1`, identity.Scope).
		Scan(&expectedPages, &expectedCount, &membership, &pagesDigest); err != nil {
		return err
	}
	if expectedPages != nil {
		if *expectedPages == manifest.PageCount && *expectedCount == manifest.DocumentCount && *membership == manifest.MembershipSHA256 && *pagesDigest == manifest.PagesSHA256 {
			return nil
		}
		return fmt.Errorf("%w: immutable end mismatch", ErrCatalogConflict)
	}
	var count, documents int64
	var maxIndex int32
	if err := tx.QueryRow(ctx, `SELECT count(*), COALESCE(sum(document_count),0)::bigint,
		COALESCE(max(page_index),-1) FROM search.catalog_snapshot_pages WHERE scope=$1`, identity.Scope).
		Scan(&count, &documents, &maxIndex); err != nil {
		return err
	}
	if count > int64(manifest.PageCount) || documents > manifest.DocumentCount || maxIndex >= manifest.PageCount {
		return fmt.Errorf("%w: stored pages exceed manifest", ErrCatalogConflict)
	}
	_, err := tx.Exec(ctx, `UPDATE search.catalog_snapshots SET page_count=$2, document_count=$3,
		membership_sha256=$4, pages_sha256=$5 WHERE scope=$1`, identity.Scope, manifest.PageCount,
		manifest.DocumentCount, manifest.MembershipSHA256, manifest.PagesSHA256)
	return err
}
