-- name: SuggestTitlePrefix :many
-- Doc-derived completions: distinct eligible titles whose lowercase form starts
-- with the (already normalized) prefix. Uses the lower(title) text_pattern_ops
-- index. @prefix must be the normalized prefix with a trailing '%'.
SELECT DISTINCT ON (lower(d.title)) d.title, d.views
FROM search.documents d
WHERE d.eligible
  AND (NOT @hide_sensitive::bool OR NOT d.is_sensitive)
  AND lower(d.title) LIKE @prefix::text
ORDER BY lower(d.title), d.views DESC
LIMIT @lim::int;

-- name: SuggestChannelPrefix :many
SELECT d.channel_name, min(d.channel_handle)::text AS channel_handle, max(d.views)::bigint AS views
FROM search.documents d
WHERE d.eligible
  AND (NOT @hide_sensitive::bool OR NOT d.is_sensitive)
  AND d.channel_name IS NOT NULL
  AND lower(d.channel_name) LIKE @prefix::text
GROUP BY d.channel_name
ORDER BY views DESC
LIMIT @lim::int;

-- name: SuggestTagPrefix :many
SELECT t::text AS tag, count(*)::bigint AS cnt
FROM search.documents d, unnest(d.tags) AS t
WHERE d.eligible
  AND (NOT @hide_sensitive::bool OR NOT d.is_sensitive)
  AND lower(t) LIKE @prefix::text
GROUP BY t
ORDER BY cnt DESC
LIMIT @lim::int;

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
SELECT DISTINCT ON (lower(d.title)) d.title,
       similarity(lower(d.title), @q::text)::real AS sim,
       d.views
FROM search.documents d
WHERE d.eligible
  AND (NOT @hide_sensitive::bool OR NOT d.is_sensitive)
  AND similarity(lower(d.title), @q::text) >= 0.35
ORDER BY lower(d.title), sim DESC
LIMIT @lim::int;
