package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vidra/vidra-search/internal/catalog"
)

var ErrCatalogQuota = errors.New("catalog pending quota exceeded")

// StageCatalogPage persists one business-validated page without changing live
// documents. The caller owns a READ COMMITTED transaction and rolls back on any
// error, including generation admission or quota/ownership conflicts.
func StageCatalogPage(ctx context.Context, tx pgx.Tx, identity catalog.Identity, page catalog.PageDigest, documents []byte) error {
	if page.PageIndex < 0 || page.PageIndex >= catalog.MaxPages {
		return fmt.Errorf("%w: page index", ErrCatalogQuota)
	}
	canonical, digest, err := catalog.DocumentsDigest(documents)
	if err != nil || digest != page.ContentSHA256 {
		return fmt.Errorf("%w: page content digest", ErrCatalogConflict)
	}
	var members []struct {
		Video struct {
			ID uuid.UUID `json:"id"`
		} `json:"video"`
	}
	if err := json.Unmarshal(canonical, &members); err != nil || len(members) == 0 {
		return fmt.Errorf("%w: empty or invalid membership", ErrCatalogConflict)
	}
	ids := make([]uuid.UUID, len(members))
	for i := range members {
		ids[i] = members[i].Video.ID
	}
	if _, err := catalog.MembershipDigest(ids); err != nil {
		return fmt.Errorf("%w: %v", ErrCatalogConflict, err)
	}
	if err := AdmitCatalogGeneration(ctx, tx, identity); err != nil {
		return err
	}
	var existingHash string
	var identical bool
	err = tx.QueryRow(ctx, `SELECT content_sha256, documents=$4::jsonb
		FROM search.catalog_snapshot_pages WHERE scope=$1 AND generation=$2 AND page_index=$3`,
		identity.Scope, int64(identity.Generation), page.PageIndex, canonical).Scan(&existingHash, &identical)
	if err == nil {
		if existingHash == digest && identical {
			return nil // An immutable replay never consumes the budget twice.
		}
		return fmt.Errorf("%w: immutable page mismatch", ErrCatalogConflict)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	var charged, pages, count int64
	var expectedPages *int32
	var expectedCount *int64
	if err := tx.QueryRow(ctx, `SELECT canonical_bytes, page_count, document_count,
		(SELECT count(*) FROM search.catalog_snapshot_pages WHERE scope=$1),
		(SELECT COALESCE(sum(document_count),0)::bigint FROM search.catalog_snapshot_pages WHERE scope=$1)
		FROM search.catalog_snapshots WHERE scope=$1`, identity.Scope).
		Scan(&charged, &expectedPages, &expectedCount, &pages, &count); err != nil {
		return err
	}
	switch {
	case charged+int64(len(canonical)) > catalog.MaxSnapshotBytes:
		return fmt.Errorf("%w: canonical bytes", ErrCatalogQuota)
	case count+int64(len(ids)) > catalog.MaxDocuments:
		return fmt.Errorf("%w: document count", ErrCatalogQuota)
	case pages+1 > catalog.MaxPages:
		return fmt.Errorf("%w: page count", ErrCatalogQuota)
	}
	if expectedPages != nil && (page.PageIndex >= *expectedPages || count+int64(len(ids)) > *expectedCount) {
		return fmt.Errorf("%w: page exceeds declared manifest", ErrCatalogConflict)
	}
	var collision bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(
		SELECT 1 FROM search.catalog_snapshot_pages p, jsonb_array_elements(p.documents) d
		WHERE p.scope=$1 AND (d->'video'->>'id')::uuid=ANY($2::uuid[]))
		OR EXISTS(SELECT 1 FROM search.documents WHERE kind<>'local' AND video_id=ANY($2::uuid[]))`,
		identity.Scope, ids).Scan(&collision); err != nil {
		return err
	}
	if collision {
		return fmt.Errorf("%w: duplicate membership or foreign ownership", ErrCatalogConflict)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO search.catalog_snapshot_pages
		(scope, epoch, generation, page_index, documents, document_count, content_sha256)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`, identity.Scope, identity.Epoch, int64(identity.Generation),
		page.PageIndex, canonical, len(ids), digest); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE search.catalog_snapshots SET canonical_bytes=canonical_bytes+$2
		WHERE scope=$1`, identity.Scope, int64(len(canonical)))
	return err
}
