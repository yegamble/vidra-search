-- Suggestible re-evaluation: recompute search.query_aggregates.suggestible for
-- EVERY aggregate row from the currently-surviving query_log, not only for rows
-- carrying new traffic.
--
-- Why this exists. RollupQueryAggregates recomputes `suggestible` only for
-- queries present in its `batch` CTE, and that CTE is INNER JOINed — a query
-- receiving no new traffic is never revisited. Nothing else writes the flag
-- (moderation.sql aside) and no DELETE in the schema touches query_aggregates:
-- retention prunes query_log and behavior_events only. So a string that once
-- cleared the distinct-user floor stayed suggestible forever, and at the
-- retention boundary the query_log rows proving how it got there were deleted
-- while the suggestion survived. These queries make `suggestible` mean
-- "currently supported by surviving evidence" instead of "once was".
--
-- The predicate below is deliberately the SAME predicate rollups.sql applies
-- (`distinct_users >= min_users AND NOT banned`, counted over the same retention
-- window with the same anonymous session fallback). This pass is the rollup's
-- recount with the INNER JOIN removed — not a second, competing policy — so the
-- two can never disagree and the flag cannot flap between them.

-- name: QueryLogHasRowsInWindow :one
-- Reconcile-orphan guard. This repo already learned that a repair pass which
-- runs on incomplete data damages the index; an empty retention window is that
-- signature exactly. With no surviving rows every aggregate would recount to
-- zero and the pass would un-suggest the entire instance in one sweep — so the
-- caller refuses to run. EXISTS, not count(*): it stops at the first row.
SELECT EXISTS (
    SELECT 1 FROM search.query_log
    WHERE submitted_at >= @window_start AND normalized_query <> ''
)::bool AS has_rows;

-- name: PreviewSuggestibleReevaluation :many
-- The dry run. Returns the rows the pass WOULD move and the value each would
-- take, changing nothing, so an operator can measure the blast radius before
-- taking it — a remediation that silently empties autosuggest is a worse outcome
-- than the bug it closes. Keyset-paged on the primary key (@after) rather than
-- OFFSET so a full count over a large table is a bounded forward sweep.
WITH survivors AS (
    -- Exact distinct users over the retained window, mirroring rollups.sql:
    -- count(DISTINCT user_id) ignores NULLs, anonymous rows fall back to their
    -- distinct session_id.
    SELECT ql.normalized_query,
           (count(DISTINCT ql.user_id)
              + count(DISTINCT CASE WHEN ql.user_id IS NULL THEN ql.session_id END))::int AS distinct_users
    FROM search.query_log ql
    WHERE ql.submitted_at >= @window_start AND ql.normalized_query <> ''
    GROUP BY ql.normalized_query
)
SELECT qa.normalized_query,
       COALESCE(s.distinct_users, 0)::int AS distinct_users,
       (((COALESCE(s.distinct_users, 0) >= @min_users::int) AND NOT qa.banned))::bool AS suggestible
FROM search.query_aggregates qa
LEFT JOIN survivors s ON s.normalized_query = qa.normalized_query
WHERE qa.normalized_query > @after
  AND ((COALESCE(s.distinct_users, 0) >= @min_users::int) AND NOT qa.banned)
      IS DISTINCT FROM qa.suggestible
ORDER BY qa.normalized_query
LIMIT @lim::int;

-- name: ReevaluateSuggestible :execrows
-- Apply one bounded batch, in the house `:execrows` idiom the prune queries use.
--
-- Two properties make the caller's loop safe. (1) The `changed` CTE selects ONLY
-- rows whose flag actually moves, so every batch strictly shrinks the remaining
-- work and the loop converges without a cursor — a row it fixes cannot reappear.
-- (2) `banned` is absent from the SET list, so the pass cannot clear a ban: it is
-- structurally incapable of un-banning, not merely careful not to. A banned row's
-- `want` is false by construction, so it is driven to (and held at)
-- suggestible = false no matter how much traffic it receives.
--
-- distinct_users moves with the flag rather than being left at the rollup's stale
-- high-water mark: a row reading "not suggestible, 3 distinct users" would look
-- like a bug on the operator surface instead of like the evidence it is.
WITH survivors AS (
    SELECT ql.normalized_query,
           (count(DISTINCT ql.user_id)
              + count(DISTINCT CASE WHEN ql.user_id IS NULL THEN ql.session_id END))::int AS distinct_users
    FROM search.query_log ql
    WHERE ql.submitted_at >= @window_start AND ql.normalized_query <> ''
    GROUP BY ql.normalized_query
),
changed AS (
    SELECT qa.normalized_query,
           COALESCE(s.distinct_users, 0)::int AS distinct_users,
           ((COALESCE(s.distinct_users, 0) >= @min_users::int) AND NOT qa.banned) AS want
    FROM search.query_aggregates qa
    LEFT JOIN survivors s ON s.normalized_query = qa.normalized_query
    WHERE ((COALESCE(s.distinct_users, 0) >= @min_users::int) AND NOT qa.banned)
          IS DISTINCT FROM qa.suggestible
    ORDER BY qa.normalized_query
    LIMIT @lim::int
)
UPDATE search.query_aggregates qa
SET suggestible    = c.want,
    distinct_users = c.distinct_users
FROM changed c
WHERE qa.normalized_query = c.normalized_query;
