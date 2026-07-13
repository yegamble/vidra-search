-- Advanced-mode feature reads (§1.7, §1.8). Stage-1 recall returns rich per-doc
-- columns so the Go linear ranker (internal/ranking) can rerank without extra
-- round trips; the affinity/session queries supply the personalization features.

-- name: SearchAdvancedRecall :many
-- Stage-1 recall for advanced search: the simple-mode hybrid recall (full-text ∪
-- trigram title ∪ exact tag/channel) UNION the videos that earned clicks for this
-- exact query (query_video_engagement), capped at @lim (500). Each row carries the
-- text-match components and the per-(query,video) engagement counters (LEFT
-- JOINed) so stage-2 computes text_score + smoothed CTR + meaningful-watch-rate in
-- Go. Ordered by text relevance then views so the cap keeps the most relevant.
WITH q AS (
    SELECT websearch_to_tsquery('simple', @query::text) AS tsq
),
recall AS (
    SELECT d.video_id
    FROM search.documents d, q
    WHERE d.eligible
      AND (NOT @hide_sensitive::bool OR NOT d.is_sensitive)
      AND (
            d.tsv @@ q.tsq
         OR lower(d.title) % @query::text
         OR d.tags @> ARRAY[@query::text]
         OR lower(d.channel_name) = @query::text
          )
    UNION
    SELECT qve.video_id
    FROM search.query_video_engagement qve
    JOIN search.documents d ON d.video_id = qve.video_id
    WHERE qve.normalized_query = @query::text
      AND qve.clicks > 0
      AND d.eligible
      AND (NOT @hide_sensitive::bool OR NOT d.is_sensitive)
)
SELECT d.video_id, d.channel_id, d.language, d.category, d.tags,
       d.views, d.published_at, d.source_updated_at,
       ts_rank_cd(d.tsv, q.tsq)::double precision AS ts_rank,
       COALESCE(similarity(lower(d.title), @query::text), 0)::double precision AS trgm_sim,
       ( (CASE WHEN lower(d.title) = @query::text THEN 1.0 ELSE 0.0 END)
       + (CASE WHEN lower(coalesce(d.channel_name, '')) = @query::text THEN 0.5 ELSE 0.0 END)
       + (CASE WHEN @query::text = ANY(SELECT lower(x) FROM unnest(d.tags) AS x) THEN 0.5 ELSE 0.0 END)
       )::double precision AS exact_flags,
       COALESCE(qve.impressions, 0)::bigint AS impressions,
       COALESCE(qve.clicks, 0)::bigint AS clicks,
       COALESCE(qve.meaningful_watches, 0)::bigint AS meaningful_watches
FROM search.documents d
CROSS JOIN q
JOIN recall r ON r.video_id = d.video_id
LEFT JOIN search.query_video_engagement qve
       ON qve.normalized_query = @query::text AND qve.video_id = d.video_id
WHERE (sqlc.narg('tag')::text IS NULL OR sqlc.narg('tag') = ANY(d.tags))
  AND (sqlc.narg('category')::text IS NULL OR d.category = sqlc.narg('category'))
  AND (sqlc.narg('language')::text IS NULL OR d.language = sqlc.narg('language'))
ORDER BY ts_rank DESC, d.views DESC, d.video_id
LIMIT @lim::int;

-- name: NeighborAffinity :many
-- Per-candidate personal affinity: for each candidate that neighbors a video the
-- user has watched, the projection-weighted sum of neighbor scores.
SELECT n.neighbor_id AS video_id, sum(uwp.weight * n.score)::double precision AS affinity
FROM search.user_watch_projection uwp
JOIN search.item_neighbors n ON n.video_id = uwp.video_id
WHERE uwp.user_id = @user_id
  AND n.neighbor_id = ANY(@candidates::uuid[])
GROUP BY n.neighbor_id;

-- name: UserChannelAffinity :many
-- Total decayed watch weight the user has accumulated on each channel — the
-- "direct channel affinity" personalization term (and the "subscribed"-style
-- reason source, since search has no subscription data).
SELECT d.channel_id, sum(uwp.weight)::double precision AS weight
FROM search.user_watch_projection uwp
JOIN search.documents d ON d.video_id = uwp.video_id
WHERE uwp.user_id = @user_id
  AND d.channel_id IS NOT NULL
GROUP BY d.channel_id;

-- name: ListDocFeaturesByIDs :many
-- Rerank features for a candidate id set (advanced related/home): eligible (and,
-- when requested, non-sensitive) docs only. Order is not preserved.
SELECT d.video_id, d.channel_id, d.language, d.category, d.tags,
       d.views, d.published_at, d.source_updated_at
FROM search.documents d
WHERE d.eligible
  AND (NOT @hide_sensitive::bool OR NOT d.is_sensitive)
  AND d.video_id = ANY(@ids::uuid[]);

-- name: RecentlyWatchedVideos :many
-- The user's most recently watched videos (novelty demotion / hide-watched).
SELECT video_id
FROM search.user_watch_projection
WHERE user_id = @user_id
ORDER BY last_watched_at DESC, video_id
LIMIT @lim::int;

-- name: FreshLowViewEligible :many
-- The ε-greedy exploration pool: fresh, low-view eligible docs, newest first.
SELECT d.video_id, d.channel_id
FROM search.documents d
WHERE d.eligible
  AND (NOT @hide_sensitive::bool OR NOT d.is_sensitive)
  AND d.views <= @max_views::bigint
  AND (sqlc.narg('language')::text IS NULL OR d.language = sqlc.narg('language'))
ORDER BY d.published_at DESC NULLS LAST, d.video_id
LIMIT @lim::int;
