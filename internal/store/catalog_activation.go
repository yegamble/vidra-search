package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/vidra/vidra-search/internal/catalog"
)

const catalogIncoming = `WITH incoming AS MATERIALIZED (
	SELECT d AS doc, d->'video' AS v FROM search.catalog_snapshot_pages p,
	jsonb_array_elements(p.documents) d
	WHERE p.scope=$1 AND p.epoch=$2 AND p.generation=$3
) `

// CompleteCatalogSnapshot atomically projects a complete current observation and
// publishes its manifest. The caller owns a READ COMMITTED transaction and must
// roll it back on any error. Incomplete/already completed observations return false.
func CompleteCatalogSnapshot(ctx context.Context, tx pgx.Tx, id catalog.Identity) (bool, error) {
	verified, err := LoadVerifiedCatalogSnapshot(ctx, tx, id)
	if err != nil || verified == nil {
		return false, err
	}
	args := []any{id.Scope, id.Epoch, int64(id.Generation)}
	// Ownership may have changed since page admission. Validate again while the
	// catalog lock excludes every supported catalog writer, before live mutation.
	var invalid bool
	if err := tx.QueryRow(ctx, catalogIncoming+`SELECT EXISTS(SELECT 1 FROM incoming
		WHERE v->>'kind' IS DISTINCT FROM 'local' OR v->>'privacy' IS DISTINCT FROM 'public'
		OR v->>'state' IS DISTINCT FROM 'published' OR doc->>'eligible' IS DISTINCT FROM 'true'
		OR doc->'suppressed_reason' IS DISTINCT FROM 'null'::jsonb)
		OR EXISTS(SELECT 1 FROM incoming JOIN search.documents d ON d.video_id=(v->>'id')::uuid
		WHERE d.kind<>'local')`, args...).Scan(&invalid); err != nil {
		return false, err
	}
	if invalid {
		return false, fmt.Errorf("%w: ineligible or foreign projection", ErrCatalogConflict)
	}
	// Frozen counters seed new rows only: an unversioned counter stream has no
	// provable order relative to this full observation. Avoid rewriting unchanged
	// metadata, including its generated full-text vector and indexed_at.
	if _, err := tx.Exec(ctx, catalogIncoming+`INSERT INTO search.documents AS live (
		video_id,kind,channel_id,channel_handle,channel_name,owner_id,title,description,tags,
		category,language,license,duration_seconds,is_sensitive,eligible,suppressed_reason,
		views,likes,published_at,source_updated_at)
		SELECT (v->>'id')::uuid,'local',(v->>'channel_id')::uuid,v->>'channel_handle',v->>'channel_name',
		(v->>'owner_id')::uuid,v->>'title',v->>'description',
		ARRAY(SELECT jsonb_array_elements_text(COALESCE(NULLIF(v->'tags','null'::jsonb),'[]'::jsonb))),
		v->>'category',v->>'language',v->>'license',(v->>'duration_seconds')::integer,
		(v->>'is_sensitive')::boolean,true,NULL,(v->>'views')::bigint,(v->>'likes')::integer,
		(v->>'published_at')::timestamptz,COALESCE(v->>'updated_at',v->>'created_at')::timestamptz
		FROM incoming
		ON CONFLICT(video_id) DO UPDATE SET
		channel_id=EXCLUDED.channel_id,channel_handle=EXCLUDED.channel_handle,
		channel_name=EXCLUDED.channel_name,owner_id=EXCLUDED.owner_id,title=EXCLUDED.title,
		description=EXCLUDED.description,tags=EXCLUDED.tags,category=EXCLUDED.category,
		language=EXCLUDED.language,license=EXCLUDED.license,duration_seconds=EXCLUDED.duration_seconds,
		is_sensitive=EXCLUDED.is_sensitive,eligible=true,suppressed_reason=NULL,
		published_at=EXCLUDED.published_at,source_updated_at=EXCLUDED.source_updated_at,indexed_at=now()
		WHERE live.kind='local' AND ROW(live.channel_id,live.channel_handle,live.channel_name,live.owner_id,
		live.title,live.description,live.tags,live.category,live.language,live.license,live.duration_seconds,
		live.is_sensitive,live.eligible,live.suppressed_reason,live.published_at,live.source_updated_at)
		IS DISTINCT FROM ROW(EXCLUDED.channel_id,EXCLUDED.channel_handle,EXCLUDED.channel_name,EXCLUDED.owner_id,
		EXCLUDED.title,EXCLUDED.description,EXCLUDED.tags,EXCLUDED.category,EXCLUDED.language,EXCLUDED.license,
		EXCLUDED.duration_seconds,EXCLUDED.is_sensitive,EXCLUDED.eligible,EXCLUDED.suppressed_reason,
		EXCLUDED.published_at,EXCLUDED.source_updated_at)`, args...); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, catalogIncoming+`UPDATE search.documents d SET eligible=false,
		suppressed_reason='catalog_orphan',indexed_at=now() WHERE d.kind='local' AND d.eligible
		AND NOT EXISTS(SELECT 1 FROM incoming WHERE (v->>'id')::uuid=d.video_id)`, args...); err != nil {
		return false, err
	}
	m := verified.manifest
	updated, err := tx.Exec(ctx, `UPDATE search.catalog_scopes SET completed_generation=$3,
		completed_at=now(),completed_page_count=$4,completed_document_count=$5,
		completed_membership_sha256=$6,completed_pages_sha256=$7
		WHERE scope=$1 AND epoch=$2 AND highest_observed_generation=$3 AND completed_generation<$3`,
		id.Scope, id.Epoch, int64(id.Generation), m.PageCount, m.DocumentCount, m.MembershipSHA256, m.PagesSHA256)
	if err != nil {
		return false, err
	}
	if updated.RowsAffected() != 1 {
		return false, fmt.Errorf("%w: completion fence changed", ErrCatalogConflict)
	}
	_, err = tx.Exec(ctx, `DELETE FROM search.catalog_snapshots WHERE scope=$1 AND epoch=$2 AND generation=$3`, args...)
	return err == nil, err
}
