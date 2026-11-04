package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/vidra/vidra-search/internal/catalog"
)

// ReadCatalogStatus takes one MVCC snapshot of permanent completion authority
// and pending diagnostics. Highest/staging are never treated as read readiness.
// This method alone does not register or advertise the HTTP capability.
func (s *Store) ReadCatalogStatus(ctx context.Context, scope string) (catalog.Status, error) {
	status := catalog.Status{ProtocolVersion: 2, Scope: scope, SupportedScopes: []string{"local"},
		SupportedEventTypes: []string{catalog.TypeSnapshotPage, catalog.TypeSnapshotEnd},
		Limits: catalog.Limits{MaxRequestBytes: catalog.MaxRequestBytes, MaxSnapshotBytes: catalog.MaxSnapshotBytes,
			MaxDocuments: catalog.MaxDocuments, MaxPages: catalog.MaxPages},
		HighestObservedGeneration: "0", CompletedGeneration: "0"}
	if scope != "local" {
		return catalog.Status{}, fmt.Errorf("%w: unsupported scope", ErrCatalogConflict)
	}
	var highest, completed int64
	var membership, pages *string
	var pageCount int32
	var documentCount int64
	err := s.Pool.QueryRow(ctx, `SELECT s.epoch,s.highest_observed_generation,s.completed_generation,
		s.completed_at,s.completed_page_count,s.completed_document_count,
		s.completed_membership_sha256,s.completed_pages_sha256,p.created_at,
		EXISTS(SELECT 1 FROM search.documents WHERE kind<>'local' AND eligible)
		FROM search.catalog_scopes s LEFT JOIN search.catalog_snapshots p
		ON p.scope=s.scope AND p.epoch=s.epoch AND p.generation=s.highest_observed_generation
		WHERE s.scope=$1`, scope).Scan(&status.Epoch, &highest, &completed, &status.CompletedAt,
		&pageCount, &documentCount, &membership, &pages, &status.OldestStagedAt, &status.ForeignEligible)
	if errors.Is(err, pgx.ErrNoRows) {
		return status, nil
	}
	if err != nil {
		return catalog.Status{}, err
	}
	status.HighestObservedGeneration = strconv.FormatInt(highest, 10)
	status.CompletedGeneration = strconv.FormatInt(completed, 10)
	status.Ready = completed > 0
	if status.Ready {
		status.CompletedManifest = &catalog.Manifest{PageCount: pageCount, DocumentCount: documentCount,
			MembershipSHA256: *membership, PagesSHA256: *pages}
	}
	if status.OldestStagedAt != nil {
		status.StagedSnapshots = 1
	}
	return status, nil
}
