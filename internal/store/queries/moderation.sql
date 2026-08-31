-- name: BanSuggestion :exec
-- Ban a normalized query from the global suggestion stream. This and
-- UnbanSuggestion are the only writers of search.query_aggregates.banned —
-- everything else in the schema only reads it (suggestions.sql filters on it,
-- rollups.sql carries it forward), so without them the flag is unreachable
-- except by hand-written SQL against an undocumented column.
--
-- Two columns move, not one, and the second is not redundant. Migration 0006 and
-- rollups.sql both state the invariant "suggestible only when the distinct-user
-- threshold is cleared AND the query is not banned", but only the rollup ever
-- writes `suggestible`, and it recomputes it ONLY for queries that appear in its
-- new-traffic batch CTE (an INNER JOIN). Nothing decays or prunes the flag
-- either: retention deletes query_log/behavior_events, never query_aggregates. A
-- banned row whose traffic dries up would therefore keep `suggestible = true`
-- forever, leaving the documented invariant permanently violated and any future
-- reader keyed on `suggestible` alone free to serve a banned query. Writing both
-- columns here makes the ban self-consistent immediately, with no dependence on
-- a rollup pass that may never come.
--
-- Upsert rather than UPDATE so a ban can pre-empt a known-bad string that has no
-- aggregate row yet. The placeholder carries zero counts; the rollup's LEFT JOIN
-- later folds real traffic into it and preserves the ban.
INSERT INTO search.query_aggregates (normalized_query, display_query, suggestible, banned)
VALUES (@normalized_query, @normalized_query, false, true)
ON CONFLICT (normalized_query) DO UPDATE SET
    suggestible = false,
    banned      = true;

-- name: UnbanSuggestion :exec
-- Lift a ban. It deliberately does NOT set suggestible = true: eligibility for
-- instance-wide suggestion is the rollup's to grant, from a real distinct-user
-- count (the privacy threshold in docs/privacy.md). An unban that restored
-- suggestibility directly would be a way to promote a query that never cleared
-- that threshold. The query returns to the suggestion stream on the next
-- aggregates_rollup pass that sees traffic for it — and if it never gets traffic
-- again it never returns, which is the honest outcome.
--
-- Idempotent by construction (matching DeleteUserSearchHistoryEntry): unbanning
-- a query that is not banned, or has no row at all, is a no-op success, so a
-- retrying caller never sees a spurious failure.
UPDATE search.query_aggregates
SET banned = false
WHERE normalized_query = @normalized_query;

-- name: ListBannedSuggestions :many
-- The reviewable ban list, so a ban can be audited and reversed by someone who
-- did not place it. Ordered by the primary key rather than by recency: last_seen
-- moves under concurrent rollups and would make a paged read skip or repeat rows.
--
-- No index on `banned`: the ban list is an operator surface, not a hot path, and
-- the banned set is expected to be a handful of rows. A partial index would cost
-- a migration to save a sequential scan nobody is waiting on.
SELECT normalized_query, display_query, total_count, distinct_users, first_seen, last_seen
FROM search.query_aggregates
WHERE banned
ORDER BY normalized_query
LIMIT @lim::int OFFSET @off::int;
