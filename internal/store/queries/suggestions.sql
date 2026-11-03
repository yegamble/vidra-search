-- name: SuggestTitlePrefix :many
-- Doc-derived completions: distinct eligible titles whose lowercase form starts
-- with the (already normalized) prefix. Uses the lower(title) text_pattern_ops
-- index. @prefix must be the normalized prefix with a trailing '%'.
-- video_id is the representative (highest-views) eligible doc backing each title:
-- the index carries only a STATIC eligibility flag, so the gateway (vidra-core)
-- re-checks this id against the authoritative video row before the title is shown
-- (H1) — a stale/corrupted eligible flag must not leak a private title.
SELECT DISTINCT ON (lower(d.title)) d.title, d.views, d.video_id
FROM search.documents d
WHERE d.eligible
  AND (NOT @hide_sensitive::bool OR NOT d.is_sensitive)
  AND lower(d.title) LIKE @prefix::text
ORDER BY lower(d.title), d.views DESC
LIMIT @lim::int;

-- name: SuggestChannelPrefix :many
-- Keep the label, handle and representative identifier on the same source row.
-- Limit groups first so indexed representative lookups stay bounded by @lim.
SELECT candidates.channel_name, COALESCE(witness.channel_handle, '')::text AS channel_handle,
       candidates.views, witness.video_id
FROM (
    SELECT d.channel_name, max(d.views)::bigint AS views
    FROM search.documents d
    WHERE d.eligible
      AND (NOT @hide_sensitive::bool OR NOT d.is_sensitive)
      AND d.channel_name IS NOT NULL
      AND lower(d.channel_name) LIKE @prefix::text
    GROUP BY d.channel_name
    ORDER BY views DESC, d.channel_name
    LIMIT @lim::int
) candidates
CROSS JOIN LATERAL (
    SELECT d.video_id, d.channel_handle
    FROM search.documents d
    WHERE d.eligible AND (NOT @hide_sensitive::bool OR NOT d.is_sensitive)
      AND lower(d.channel_name) = lower(candidates.channel_name)
      AND d.channel_name = candidates.channel_name AND d.views = candidates.views
    ORDER BY d.video_id
    LIMIT 1
) witness
ORDER BY candidates.views DESC, candidates.channel_name, witness.video_id;

-- name: SuggestTagPrefix :many
SELECT candidates.tag, candidates.cnt, witness.video_id
FROM (
    SELECT t.tag::text AS tag, count(*)::bigint AS cnt
    FROM search.documents d, unnest(d.tags) AS t(tag)
    WHERE d.eligible
      AND (NOT @hide_sensitive::bool OR NOT d.is_sensitive)
      AND lower(t.tag) LIKE @prefix::text
    GROUP BY t.tag
    ORDER BY cnt DESC, t.tag
    LIMIT @lim::int
) candidates
CROSS JOIN LATERAL (
    SELECT d.video_id
    FROM search.documents d
    WHERE d.eligible AND (NOT @hide_sensitive::bool OR NOT d.is_sensitive)
      AND d.tags @> ARRAY[candidates.tag]
    ORDER BY d.views DESC, d.video_id
    LIMIT 1
) witness
ORDER BY candidates.cnt DESC, candidates.tag;

-- name: SuggestAggregatePrefix :many
-- The global-popularity suggestion stream (§1.6a): suggestible, non-banned
-- aggregate queries whose normalized form starts with the prefix, ordered by
-- decayed frequency. Uses the query_aggregates text_pattern_ops prefix index.
-- @prefix must be the normalized prefix with a trailing '%'.
SELECT normalized_query, display_query, decayed_freq
FROM search.query_aggregates
WHERE suggestible AND NOT banned
  AND normalized_query LIKE @prefix::text
ORDER BY decayed_freq DESC
LIMIT @lim::int;

-- name: SuggestUserHistoryPrefix :many
-- The personal suggestion stream (§1.6c): the signed-in user's own non-hidden
-- recent queries matching the prefix, most-recent first.
SELECT normalized_query, display_query, last_used_at, use_count
FROM search.user_search_history
WHERE user_id = @user_id AND NOT hidden
  AND normalized_query LIKE @prefix::text
ORDER BY last_used_at DESC
LIMIT @lim::int;

-- name: SuggestTitleFuzzy :many
-- Typo fallback: trigram-similar titles, used only when exact-prefix results are
-- short of the requested limit. Threshold 0.35 (algorithms report).
-- video_id (representative eligible doc) rides along so the gateway can re-check
-- the fuzzy-matched title against the authoritative video row (H1), exactly as
-- the exact-prefix stream does.
SELECT DISTINCT ON (lower(d.title)) d.title,
       similarity(lower(d.title), @q::text)::real AS sim,
       d.views, d.video_id
FROM search.documents d
WHERE d.eligible
  AND (NOT @hide_sensitive::bool OR NOT d.is_sensitive)
  -- `%` recalls trigram candidates via the lower(title) GIN index (threshold
  -- pg_trgm.similarity_threshold=0.3); the explicit >= 0.35 then refines to the
  -- algorithms-report cutoff on that small candidate set (was a full seq scan).
  AND lower(d.title) % @q::text
  AND similarity(lower(d.title), @q::text) >= 0.35
ORDER BY lower(d.title), sim DESC
LIMIT @lim::int;
