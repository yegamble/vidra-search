-- Suggestible re-evaluation: recompute search.query_aggregates.suggestible AND
-- the popularity counters for EVERY aggregate row from the currently-surviving
-- query_log, not only for rows carrying new traffic.
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
-- window with the same anonymous subject, and the same session fallback for rows
-- carrying no subject). This pass is the rollup's recount with the INNER JOIN
-- removed — not a second, competing policy — so the two can never disagree and
-- the flag cannot flap between them.
--
-- That sharing is load-bearing and it is easy to break by halves. This pass is
-- what re-applies the floor to EVERY aggregate row, so it is also where a
-- one-sided edit does its damage: give the rollup a stricter count than this one
-- and every row the rollup demotes is promoted straight back the next night, for
-- ever. Both files carry the same counting expression byte-for-byte;
-- TestFloorPredicateIsSharedByRollupAndReevaluation asserts that at the source
-- level and TestIntegrationRollupAndReevaluationAgreeOnSubjects asserts it
-- behaviourally, on a fixture that straddles the floor in both directions.
--
-- The SAME argument now carries total_count / decayed_freq. The rollup recomputes
-- them from the retained window for any query with new traffic; a query that goes
-- QUIET is never in its batch, so this daily pass is the only thing that can
-- reach it — and until it did, "the anonymous popularity totals this site keeps
-- are recomputed without you within a day" was false for exactly the query a user
-- had just cleared and then never searched again. decayed_freq is autosuggest's
-- sort key, so this is a served number, not bookkeeping.
--
-- FIRST PASS AFTER UPGRADE. Before this change these two columns were cumulative
-- and could only ever grow, so on an instance with any history behind it the
-- first pass rewrites most of the table and reports a large `changed`. That is
-- the correction landing, not a fault. SEARCH_REEVAL_DRY_RUN=true shows the size
-- of it first, which is what the dry run is for.

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
    -- count(DISTINCT user_id) ignores NULLs, anonymous rows count core's
    -- server-derived subject_id and fall back to session_id only when the row
    -- carries no subject. THIS EXPRESSION IS SHARED WITH rollups.sql — change
    -- both or neither.
    SELECT ql.normalized_query,
           (count(DISTINCT ql.user_id)
              + count(DISTINCT CASE WHEN ql.user_id IS NULL THEN COALESCE(ql.subject_id, ql.session_id) END))::int AS distinct_users,
           count(*)::bigint     AS total_count,
           min(ql.submitted_at) AS first_seen,
           max(ql.submitted_at) AS last_seen
    FROM search.query_log ql
    WHERE ql.submitted_at >= @window_start AND ql.normalized_query <> ''
    GROUP BY ql.normalized_query
)
SELECT qa.normalized_query,
       COALESCE(s.distinct_users, 0)::int AS distinct_users,
       COALESCE(s.total_count, 0)::bigint AS total_count,
       (((COALESCE(s.distinct_users, 0) >= @min_users::int) AND NOT qa.banned))::bool AS suggestible
FROM search.query_aggregates qa
LEFT JOIN survivors s ON s.normalized_query = qa.normalized_query
WHERE qa.normalized_query > @after
  AND (((COALESCE(s.distinct_users, 0) >= @min_users::int) AND NOT qa.banned)
       IS DISTINCT FROM qa.suggestible
   -- Two exact integers decide whether a row moves. decayed_freq is a float and
   -- is deliberately NOT in this predicate: comparing one for equality every pass
   -- is how a repair loop starts churning on rounding. It rides along in the SET
   -- list instead, and it cannot drift undetected — it is a function of the same
   -- rows total_count counts, and any query whose rows change without changing
   -- their COUNT necessarily received new traffic, which puts it in the 1-minute
   -- rollup's batch.
   OR COALESCE(s.total_count, 0) IS DISTINCT FROM qa.total_count
   OR COALESCE(s.distinct_users, 0) IS DISTINCT FROM qa.distinct_users)
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
-- like a bug on the operator surface instead of like the evidence it is. The same
-- now goes for total_count / decayed_freq: they are recomputed here from the same
-- survivors, so the whole row means "currently supported by surviving evidence"
-- rather than one column meaning that and three meaning "once was".
--
-- first_seen / last_seen COALESCE back to the row's own value when nothing
-- survives, because they are NOT NULL and there is no honest zero for a
-- timestamp; the count beside them is 0, which is the honest statement.
WITH survivors AS (
    SELECT ql.normalized_query,
           (count(DISTINCT ql.user_id)
              + count(DISTINCT CASE WHEN ql.user_id IS NULL THEN COALESCE(ql.subject_id, ql.session_id) END))::int AS distinct_users,
           count(*)::bigint     AS total_count,
           min(ql.submitted_at) AS first_seen,
           max(ql.submitted_at) AS last_seen
    FROM search.query_log ql
    WHERE ql.submitted_at >= @window_start AND ql.normalized_query <> ''
    GROUP BY ql.normalized_query
),
freq AS (
    -- THIS EXPRESSION IS SHARED WITH rollups.sql — change both or neither. The
    -- rollup writes it for queries with new traffic, this pass for every row;
    -- if the two ever disagree, autosuggest's ORDER flips on every cycle between
    -- the minute pass and the nightly one.
    SELECT a.normalized_query,
           sum(power(2, - GREATEST(0, EXTRACT(EPOCH FROM (a.last_seen - ql.submitted_at)))
                        / @half_life_seconds::double precision))::double precision AS decayed_freq
    FROM survivors a
    JOIN search.query_log ql ON ql.normalized_query = a.normalized_query
        AND ql.submitted_at >= @window_start
    GROUP BY a.normalized_query
),
changed AS (
    SELECT qa.normalized_query,
           COALESCE(s.distinct_users, 0)::int AS distinct_users,
           COALESCE(s.total_count, 0)::bigint AS total_count,
           COALESCE(f.decayed_freq, 0)::double precision AS decayed_freq,
           COALESCE(s.first_seen, qa.first_seen) AS first_seen,
           COALESCE(s.last_seen, qa.last_seen) AS last_seen,
           ((COALESCE(s.distinct_users, 0) >= @min_users::int) AND NOT qa.banned) AS want
    FROM search.query_aggregates qa
    LEFT JOIN survivors s ON s.normalized_query = qa.normalized_query
    LEFT JOIN freq f ON f.normalized_query = qa.normalized_query
    WHERE (((COALESCE(s.distinct_users, 0) >= @min_users::int) AND NOT qa.banned)
           IS DISTINCT FROM qa.suggestible
       -- Exact integers only; see PreviewSuggestibleReevaluation for why
       -- decayed_freq is not a member of this predicate.
       OR COALESCE(s.total_count, 0) IS DISTINCT FROM qa.total_count
       OR COALESCE(s.distinct_users, 0) IS DISTINCT FROM qa.distinct_users)
    ORDER BY qa.normalized_query
    LIMIT @lim::int
)
UPDATE search.query_aggregates qa
SET suggestible    = c.want,
    distinct_users = c.distinct_users,
    total_count    = c.total_count,
    decayed_freq   = c.decayed_freq,
    first_seen     = c.first_seen,
    last_seen      = c.last_seen
FROM changed c
WHERE qa.normalized_query = c.normalized_query;
