-- name: RelatedSameChannel :many
-- Most recent other videos from the same channel (related "similar" seed).
SELECT d.video_id, d.channel_id
FROM search.documents d
WHERE d.eligible
  AND (NOT @hide_sensitive::bool OR NOT d.is_sensitive)
  AND d.channel_id = @channel_id
  AND d.video_id <> @video_id
ORDER BY d.published_at DESC NULLS LAST, d.video_id
LIMIT @lim::int;

-- name: RelatedByOverlap :many
-- Videos sharing tags/category/language with the seed, scored by overlap size.
SELECT d.video_id, d.channel_id,
    ( (CASE WHEN d.category IS NOT NULL AND d.category = sqlc.narg('category') THEN 1 ELSE 0 END)
    + (CASE WHEN d.language IS NOT NULL AND d.language = sqlc.narg('language') THEN 1 ELSE 0 END)
    + cardinality(ARRAY(SELECT unnest(d.tags) INTERSECT SELECT unnest(@tags::text[])))
    )::int AS overlap
FROM search.documents d
WHERE d.eligible
  AND (NOT @hide_sensitive::bool OR NOT d.is_sensitive)
  AND d.video_id <> @video_id
  AND (
        d.category = sqlc.narg('category')
     OR d.language = sqlc.narg('language')
     OR d.tags && @tags::text[]
      )
ORDER BY overlap DESC, d.views DESC, d.published_at DESC NULLS LAST, d.video_id
LIMIT @lim::int;

-- name: PopularEligible :many
-- Top eligible videos by views, excluding a caller-supplied set (self + picks).
SELECT d.video_id, d.channel_id
FROM search.documents d
WHERE d.eligible
  AND (NOT @hide_sensitive::bool OR NOT d.is_sensitive)
  AND (@exclude::uuid[] IS NULL OR d.video_id <> ALL(@exclude::uuid[]))
ORDER BY d.views DESC, d.published_at DESC NULLS LAST, d.video_id
LIMIT @lim::int;

-- name: HomeTrending :many
-- Hacker-News-style gravity: views / (hours_since_published + 2)^1.5. Same
-- language is preferred first (language-aware) without excluding other langs.
SELECT d.video_id, d.channel_id,
    ( d.views / power(EXTRACT(EPOCH FROM (now() - COALESCE(d.published_at, d.source_updated_at))) / 3600.0 + 2, 1.5) )::double precision AS score
FROM search.documents d
WHERE d.eligible
  AND (NOT @hide_sensitive::bool OR NOT d.is_sensitive)
  AND (@exclude::uuid[] IS NULL OR d.video_id <> ALL(@exclude::uuid[]))
ORDER BY (sqlc.narg('language')::text IS NOT NULL AND d.language = sqlc.narg('language')) DESC,
         score DESC, d.video_id
LIMIT @lim::int;

-- name: HomeRecent :many
-- Freshest eligible videos, same-language preferred first.
SELECT d.video_id, d.channel_id
FROM search.documents d
WHERE d.eligible
  AND (NOT @hide_sensitive::bool OR NOT d.is_sensitive)
  AND (@exclude::uuid[] IS NULL OR d.video_id <> ALL(@exclude::uuid[]))
ORDER BY (sqlc.narg('language')::text IS NOT NULL AND d.language = sqlc.narg('language')) DESC,
         d.published_at DESC NULLS LAST, d.video_id
LIMIT @lim::int;
