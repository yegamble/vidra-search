-- name: SearchSimple :many
-- Simple-mode hybrid search (§1.7). Candidate recall unions full-text
-- (websearch_to_tsquery), trigram title similarity, and exact tag/channel
-- matches; the score blends ts_rank_cd, trigram similarity, exact-match flags,
-- log-normalized views, and a 30-day freshness half-life. Static eligibility +
-- hide_sensitive + optional tag/category/language filters are applied in SQL.
-- Order is fully deterministic: score DESC, then newest, then id.
WITH q AS (
    SELECT websearch_to_tsquery('simple', @query::text) AS tsq
)
SELECT d.video_id,
    ( 0.5 * ts_rank_cd(d.tsv, q.tsq)
    + 0.2 * COALESCE(similarity(lower(d.title), @query::text), 0)
    + 0.1 * (
        (CASE WHEN lower(d.title) = @query::text THEN 1.0 ELSE 0.0 END)
      + (CASE WHEN lower(coalesce(d.channel_name, '')) = @query::text THEN 0.5 ELSE 0.0 END)
      + (CASE WHEN @query::text = ANY(SELECT lower(x) FROM unnest(d.tags) AS x) THEN 0.5 ELSE 0.0 END)
      )
    + 0.1 * (ln(1 + d.views) / 20.0)
    + 0.1 * exp(-ln(2) * (EXTRACT(EPOCH FROM (now() - COALESCE(d.published_at, d.source_updated_at))) / 2592000.0))
    )::double precision AS score
FROM search.documents d, q
WHERE d.eligible
  AND (NOT @hide_sensitive::bool OR NOT d.is_sensitive)
  AND (
        d.tsv @@ q.tsq
     OR lower(d.title) % @query::text
     OR d.tags @> ARRAY[@query::text]
     OR lower(d.channel_name) = @query::text
      )
  AND (sqlc.narg('tag')::text IS NULL OR sqlc.narg('tag') = ANY(d.tags))
  AND (sqlc.narg('category')::text IS NULL OR d.category = sqlc.narg('category'))
  AND (sqlc.narg('language')::text IS NULL OR d.language = sqlc.narg('language'))
ORDER BY score DESC, d.published_at DESC NULLS LAST, d.video_id
LIMIT @lim::int OFFSET @off::int;
