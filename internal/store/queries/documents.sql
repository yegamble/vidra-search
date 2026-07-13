-- name: UpsertDocument :exec
-- Insert or refresh a document from a video.upsert (or reconcile.page) event.
-- eligible + suppressed_reason are derived in Go from privacy/state. On a
-- reconcile page the caller passes reconcile_run_id to stamp the row; on a
-- normal upsert it passes NULL and the existing stamp is preserved (COALESCE),
-- so a later reconcile.end only suppresses rows the current run never touched.
INSERT INTO search.documents (
    video_id, kind, channel_id, channel_handle, channel_name, owner_id,
    title, description, tags, category, language, duration_seconds,
    is_sensitive, eligible, suppressed_reason, views, likes,
    published_at, source_updated_at, indexed_at, reconcile_run_id
) VALUES (
    @video_id, @kind, @channel_id, @channel_handle, @channel_name, @owner_id,
    @title, @description, @tags, @category, @language, @duration_seconds,
    @is_sensitive, @eligible, @suppressed_reason, @views, @likes,
    @published_at, @source_updated_at, now(), @reconcile_run_id
)
ON CONFLICT (video_id) DO UPDATE SET
    kind              = EXCLUDED.kind,
    channel_id        = EXCLUDED.channel_id,
    channel_handle    = EXCLUDED.channel_handle,
    channel_name      = EXCLUDED.channel_name,
    owner_id          = EXCLUDED.owner_id,
    title             = EXCLUDED.title,
    description       = EXCLUDED.description,
    tags              = EXCLUDED.tags,
    category          = EXCLUDED.category,
    language          = EXCLUDED.language,
    duration_seconds  = EXCLUDED.duration_seconds,
    is_sensitive      = EXCLUDED.is_sensitive,
    eligible          = EXCLUDED.eligible,
    suppressed_reason = EXCLUDED.suppressed_reason,
    views             = EXCLUDED.views,
    likes             = EXCLUDED.likes,
    published_at      = EXCLUDED.published_at,
    source_updated_at = EXCLUDED.source_updated_at,
    indexed_at        = now(),
    reconcile_run_id  = COALESCE(EXCLUDED.reconcile_run_id, search.documents.reconcile_run_id);

-- name: SuppressDocument :exec
-- Hard visibility kill for a single video (video.suppress). Idempotent: a row
-- that does not exist yet is inserted as an ineligible placeholder so a suppress
-- that races ahead of the upsert still hides the video once it arrives.
INSERT INTO search.documents (video_id, title, source_updated_at, eligible, suppressed_reason)
VALUES (@video_id, '', now(), false, @reason)
ON CONFLICT (video_id) DO UPDATE SET
    eligible          = false,
    suppressed_reason = @reason,
    indexed_at        = now();

-- name: UpdateDocumentStats :exec
UPDATE search.documents
SET views = @views, likes = @likes, indexed_at = now()
WHERE video_id = @video_id;

-- name: UpdateChannelDocuments :exec
-- Denormalize a channel.upsert onto every document owned by that channel.
UPDATE search.documents
SET channel_handle = @channel_handle,
    channel_name   = @channel_name,
    owner_id       = @owner_id,
    indexed_at     = now()
WHERE channel_id = @channel_id;

-- name: SuppressChannelDocuments :exec
UPDATE search.documents
SET eligible = false, suppressed_reason = @reason, indexed_at = now()
WHERE channel_id = @channel_id;

-- name: SuppressOwnerDocuments :exec
-- user.suppress with unlisted=true: hide the owner's currently-VISIBLE documents.
-- The `AND eligible` guard is essential: without it this would stamp
-- suppressed_reason='owner_unlisted' onto docs already hidden for another reason
-- (deleted/blocked/private), and RestoreOwnerDocuments would then wrongly
-- re-enable them on relist. Only docs this stamp actually hid are later restored.
UPDATE search.documents
SET eligible = false, suppressed_reason = @reason, indexed_at = now()
WHERE owner_id = @owner_id AND eligible;

-- name: RestoreOwnerDocuments :exec
-- user.suppress with unlisted=false: undo ONLY the owner-unlisted suppression.
-- Documents hidden for another reason (deleted, blocked, private) stay hidden —
-- we cannot re-derive their true eligibility without the source privacy/state.
UPDATE search.documents
SET eligible = true, suppressed_reason = NULL, indexed_at = now()
WHERE owner_id = @owner_id AND suppressed_reason = @reason;

-- name: SuppressReconcileOrphans :exec
-- After reconcile.end: hide eligible LOCAL documents the current run never
-- stamped — they no longer exist (or are no longer public) at the source.
UPDATE search.documents
SET eligible = false, suppressed_reason = 'reconcile_orphan', indexed_at = now()
WHERE kind = 'local'
  AND eligible
  AND reconcile_run_id IS DISTINCT FROM @reconcile_run_id;

-- name: GetDocument :one
SELECT video_id, kind, channel_id, channel_handle, channel_name, owner_id,
       title, description, tags, category, language, duration_seconds,
       is_sensitive, eligible, suppressed_reason, views, likes,
       published_at, source_updated_at, indexed_at, reconcile_run_id
FROM search.documents
WHERE video_id = @video_id;

-- name: CountDocumentsByEligibility :many
SELECT eligible, count(*)::bigint AS count
FROM search.documents
GROUP BY eligible;
