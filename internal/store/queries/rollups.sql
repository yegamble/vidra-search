-- Worker queries (W2): cursors, aggregate/engagement rollups, meaningful-watch
-- derivation, sessionizer reads, and retention.

-- name: GetWorkerCursor :one
SELECT cursor_pos FROM search.worker_cursors WHERE cursor_name = @cursor_name;

-- name: SetWorkerCursor :exec
INSERT INTO search.worker_cursors (cursor_name, cursor_pos, updated_at)
VALUES (@cursor_name, @cursor_pos, now())
ON CONFLICT (cursor_name) DO UPDATE SET cursor_pos = EXCLUDED.cursor_pos, updated_at = now();

-- name: MaxQueryLogID :one
SELECT COALESCE(max(id), 0)::bigint FROM search.query_log;

-- name: MaxBehaviorEventID :one
SELECT COALESCE(max(id), 0)::bigint FROM search.behavior_events;

-- name: MaxSettledQueryLogID :one
-- Highest query_log id whose abandonment/reformulation window has closed. Because
-- id and submitted_at rise together, this is a clean high-water mark below which
-- every row is settled.
SELECT COALESCE(max(id), 0)::bigint FROM search.query_log WHERE submitted_at <= @cutoff;

-- name: RollupQueryAggregates :exec
-- Refresh query_aggregates for the queries carrying new query_log rows (id in
-- (cursor, maxid]). display_query is the most recent display form; suggestible
-- clears the min distinct-user threshold and is never true for a banned query.
--
-- THE CURSOR SELECTS WHICH QUERIES TO REFRESH; IT DOES NOT ACCUMULATE ANY VALUE.
-- Every number written here — total_count, decayed_freq, distinct_users,
-- first_seen, last_seen — is recomputed from the query_log rows this instance
-- STILL HOLDS inside the retention window, so the row is exactly what a reader
-- recomputing from surviving evidence would get.
--
-- It used to be half that. distinct_users was recounted, but total_count was
-- `previous + delta` and decayed_freq was decay-then-increment, both folded
-- forward by the cursor and never pruned. So a search that retention deleted, or
-- that a user's "Clear all" removed, kept counting for ever — and decayed_freq is
-- the ORDER of the aggregate suggestion stream, so deleted searches kept buying a
-- completion its rank. That is the shape vidra-search#36 removed from
-- co-visitation (cumulative co_watch / co_search counters nothing pruned); the
-- owner's ruling there — support counters carry the same retention the floor
-- does — applies here verbatim. Recomputing costs nothing extra: the recount CTE
-- was already scanning exactly these rows for the floor.
--
-- decayed_freq is anchored on the query's own last_seen (the newest SURVIVING
-- row), which is what decay-then-increment approximated. Anchoring on now()
-- instead would be a relevance change, not a retention one: every row would then
-- have to be rewritten on every pass to stay comparable. Anchored this way the
-- value is a pure function of the retained rows, so two passes over an unchanged
-- ledger produce the same number.
--
-- first_seen / last_seen are recomputed the same way and no longer LEAST/GREATEST
-- accumulations. They must be: last_seen is decayed_freq's anchor, so a last_seen
-- that outlives its row would anchor the sum on evidence that is gone.
--
-- The anonymous half of that floor counts subject_id — core's server-derived,
-- day-scoped, address-keyed pseudonym — because session_id is a client-supplied
-- header, so rotating it minted unlimited identities and cleared the default
-- floor of 3 from one request loop. COALESCE keeps session_id as the fallback for
-- rows that carry no subject: every row written before migration 0016 has
-- subject_id IS NULL, and counting the subject alone would drop their evidence
-- the moment the column landed — the daily re-evaluation pass would then
-- un-suggest an instance's whole autosuggest corpus in one sweep. Those rows age
-- out at the retention horizon; see docs/operations.md for the measurement that
-- says when the COALESCE can be dropped.
WITH batch AS (
    -- WHICH queries to refresh, and nothing else. The counts this CTE used to
    -- contribute (`delta`) are gone: a batch count is a count of rows that
    -- arrived, which is not a count of rows that survive.
    SELECT ql.normalized_query
    FROM search.query_log ql
    WHERE ql.id > @from_id AND ql.id <= @maxid AND ql.normalized_query <> ''
    GROUP BY ql.normalized_query
),
recount AS (
    -- Exact distinct users: count(DISTINCT user_id) ignores NULLs automatically;
    -- anonymous rows count core's server-derived subject_id, falling back to
    -- session_id only when the row carries no subject (see the note above
    -- RollupQueryAggregates). FILTER is avoided deliberately (it trips sqlc
    -- 1.31.1's named-parameter editor here).
    --
    -- THIS EXPRESSION IS SHARED. reevaluation.sql applies the identical count
    -- with the batch INNER JOIN removed, so the rollup and the daily
    -- re-evaluation pass can never disagree. Change it here, change it there,
    -- in the same commit — a divergence makes `suggestible` flap between the two
    -- passes on every cycle. TestIntegrationRollupAndReevaluationAgreeOnSubjects
    -- and TestFloorPredicateIsSharedByRollupAndReevaluation pin that.
    SELECT b.normalized_query,
           (count(DISTINCT ql.user_id)
              + count(DISTINCT CASE WHEN ql.user_id IS NULL THEN COALESCE(ql.subject_id, ql.session_id) END))::int AS distinct_users,
           count(*)::bigint     AS total_count,
           min(ql.submitted_at) AS first_seen,
           max(ql.submitted_at) AS last_seen
    FROM batch b
    JOIN search.query_log ql ON ql.normalized_query = b.normalized_query
        AND ql.submitted_at >= @window_start
    GROUP BY b.normalized_query
),
freq AS (
    -- The recency-weighted sum, anchored on the row's own last_seen so the value
    -- is a function of the retained rows and of nothing else (not of now(), not
    -- of what the previous pass happened to store). A second grouping over the
    -- SAME rows recount just scanned, because an aggregate cannot reference
    -- another aggregate of its own group.
    --
    -- THIS EXPRESSION IS SHARED with reevaluation.sql, exactly as the floor count
    -- above is: the daily pass is this computation with the batch INNER JOIN
    -- removed. Change it here, change it there, in the same commit — a divergence
    -- makes decayed_freq (and therefore autosuggest's ORDER) flip between the two
    -- passes on every cycle. TestDecayedFreqSumIsSharedByRollupAndReevaluation
    -- pins it at the source level.
    SELECT a.normalized_query,
           sum(power(2, - GREATEST(0, EXTRACT(EPOCH FROM (a.last_seen - ql.submitted_at)))
                        / @half_life_seconds::double precision))::double precision AS decayed_freq
    FROM recount a
    JOIN search.query_log ql ON ql.normalized_query = a.normalized_query
        AND ql.submitted_at >= @window_start
    GROUP BY a.normalized_query
),
display AS (
    SELECT DISTINCT ON (ql.normalized_query) ql.normalized_query, ql.display_query
    FROM search.query_log ql
    JOIN batch b ON b.normalized_query = ql.normalized_query
    ORDER BY ql.normalized_query, ql.submitted_at DESC, ql.id DESC
)
-- The full new row is built in the SELECT, so the ON CONFLICT clause is a trivial
-- column-wise overwrite. The aggregates_rollup worker is a single writer inside
-- one transaction, so the read-compute-upsert is race-free. The existing
-- aggregate is still LEFT JOINed, but now for `banned` ALONE — the one column
-- that is a moderator's decision rather than a fact about the ledger, and which
-- this pass must therefore carry forward rather than recompute.
INSERT INTO search.query_aggregates
    (normalized_query, display_query, total_count, distinct_users, decayed_freq, first_seen, last_seen, suggestible, banned)
SELECT b.normalized_query,
       d.display_query,
       r.total_count,
       r.distinct_users,
       f.decayed_freq,
       r.first_seen,
       r.last_seen,
       (r.distinct_users >= @min_users::int) AND NOT COALESCE(qa.banned, false),
       COALESCE(qa.banned, false)
FROM batch b
JOIN recount r ON r.normalized_query = b.normalized_query
JOIN freq f ON f.normalized_query = b.normalized_query
JOIN display d ON d.normalized_query = b.normalized_query
LEFT JOIN search.query_aggregates qa ON qa.normalized_query = b.normalized_query
ON CONFLICT (normalized_query) DO UPDATE SET
    display_query  = EXCLUDED.display_query,
    total_count    = EXCLUDED.total_count,
    distinct_users = EXCLUDED.distinct_users,
    decayed_freq   = EXCLUDED.decayed_freq,
    first_seen     = EXCLUDED.first_seen,
    last_seen      = EXCLUDED.last_seen,
    suggestible    = EXCLUDED.suggestible,
    banned         = EXCLUDED.banned;

-- name: DeriveMeaningfulWatch :many
-- Derive synthetic video.meaningful_watch rows from qualifying video.watch_progress
-- events (position >= mw_seconds OR >= mw_pct% of duration). The event_id is a
-- deterministic uuid_generate_v5 over (subject, video, day) so re-derivation is a
-- no-op. The query is attributed to the latest search-context query for that
-- (session, video). Returns only the rows actually inserted so the worker can
-- apply the watch-projection weight and trend:v bump exactly once.
INSERT INTO search.behavior_events
    (event_id, type, user_id, session_id, normalized_query, video_id, occurred_at, props)
SELECT
    uuid_generate_v5('6ba7b814-9dad-11d1-80b4-00c04fd430c8'::uuid,
        'mw|' || COALESCE(wp.user_id::text, wp.session_id, 'anon') || '|' || wp.video_id::text
             || '|' || to_char(wp.occurred_at AT TIME ZONE 'UTC', 'YYYYMMDD')),
    'video.meaningful_watch',
    wp.user_id, wp.session_id,
    (SELECT be.normalized_query FROM search.behavior_events be
      WHERE be.session_id IS NOT DISTINCT FROM wp.session_id
        AND be.video_id = wp.video_id
        AND be.type IN ('search.result_clicked', 'video.play_started')
        AND be.normalized_query IS NOT NULL
        AND be.occurred_at <= wp.occurred_at
      ORDER BY be.occurred_at DESC LIMIT 1),
    wp.video_id, wp.occurred_at,
    -- subject_id is carried over from the watch_progress row this is derived
    -- from. video.meaningful_watch is one of the two event types the
    -- co-visitation k-anonymity floor counts, and unlike every other event it
    -- counts, this one is SYNTHESISED here — its props are whatever this object
    -- builds. A derived row with no subject falls through to
    -- COALESCE(subject_id, session_id) and is counted under the client-controlled
    -- session id, so an anonymous actor rotating X-Vidra-Session would present as
    -- N distinct "people" behind a pair whose play_started half correctly counted
    -- one.
    --
    -- Be honest about what this does today: NOTHING. vidra-core emits
    -- video.watch_progress only from the authenticated PUT
    -- /videos/:id/watch-progress route, always with a user_id, and the type is not
    -- on the public POST /search/events allowlist — so no anonymous watch_progress
    -- exists to carry a subject, this expression copies NULL, and the floor counts
    -- the user_id. It is a forward guard, and a cheap one: the day that route
    -- accepts an anonymous beacon or the type joins the allowlist, the derived row
    -- would silently start counting a forgeable identity with nothing failing.
    -- TestIntegrationCovisFloorCountsSubjectsOnDerivedMeaningfulWatches drives
    -- that shape directly and fails without this line.
    jsonb_build_object('allow_history', COALESCE((wp.props->>'allow_history')::boolean, false),
                       'derived_from', 'watch_progress',
                       'subject_id', wp.props->>'subject_id')
FROM search.behavior_events wp
WHERE wp.type = 'video.watch_progress'
  AND wp.id > @cursor AND wp.id <= @maxid
  AND wp.video_id IS NOT NULL
  AND (
        COALESCE((wp.props->>'position_seconds')::double precision, 0) >= @mw_seconds::double precision
     OR ( (wp.props->>'duration_seconds') IS NOT NULL
          AND (wp.props->>'duration_seconds')::double precision > 0
          AND COALESCE((wp.props->>'position_seconds')::double precision, 0)
                >= (wp.props->>'duration_seconds')::double precision * @mw_fraction::double precision )
      )
ON CONFLICT (event_id) DO NOTHING
RETURNING event_id, user_id, session_id, video_id, normalized_query, props;

-- name: ClearQueryVideoEngagement :exec
-- Step 1 of the engagement rebuild, in the same idiom (and for the same reason)
-- as ClearCovisNeighbors: dropping first is what lets a counter FALL. Both steps
-- run in the engagement worker's transaction, so a crash leaves the previous
-- table in place rather than an empty one.
DELETE FROM search.query_video_engagement;

-- name: RebuildQueryVideoEngagement :exec
-- Step 2: recompute every per-(query, video) counter from the CURRENTLY RETAINED
-- behavior_events. One grouped scan, no cursor, no accumulation.
--
-- It used to be a cursor fold — `existing + delta` over (cursor, maxid] — which
-- nothing ever pruned. That is the exact shape vidra-search#36 removed from
-- co-visitation, and the ruling behind it (a support counter carries the same
-- retention its evidence does) applies here unchanged. This table is READ LIVE:
-- SearchAdvancedRecall recalls candidates on `clicks > 0` for the query and feeds
-- impressions/clicks/meaningful_watches into the stage-2 CTR and
-- meaningful-watch-rate features. So a deleted click did not merely sit in a
-- disused table — it kept recalling and ranking a video, and a user's "Clear all"
-- could not reach it.
--
-- Cost: ONE grouped scan of the retained impression/click/watch ledger per pass
-- (default every 5 min), served by behavior_events_type_occurred_idx. That is
-- strictly less work than covis_rollup, which self-joins the same ledger TWICE
-- every 15 min. Like covis it is bounded by EVENT_RETENTION_DAYS rather than by
-- lifetime volume. The DELETE + INSERT rewrites the whole (usually small) table
-- each pass; autovacuum reclaims it, and docs/operations.md names the knobs if it
-- ever stops keeping up.
--
-- No time predicate here, deliberately, for the same reason covis has none: "the
-- retained ledger" is whatever the retention worker has left, so this reads the
-- table as it stands rather than second-guessing it with a window of its own.
INSERT INTO search.query_video_engagement
    (normalized_query, video_id, impressions, clicks, meaningful_watches, updated_at)
SELECT be.normalized_query, be.video_id,
       count(*) FILTER (WHERE be.type = 'video.impression'),
       count(*) FILTER (WHERE be.type = 'search.result_clicked'),
       count(*) FILTER (WHERE be.type = 'video.meaningful_watch'),
       now()
FROM search.behavior_events be
WHERE be.normalized_query IS NOT NULL
  AND be.video_id IS NOT NULL
  AND be.type IN ('video.impression', 'search.result_clicked', 'video.meaningful_watch')
GROUP BY be.normalized_query, be.video_id;

-- name: ListQueryLogRange :many
-- Settled query_log rows in the sessionizer's cursor range (session-scoped only),
-- ordered so a session's queries are contiguous and time-ordered.
SELECT id, normalized_query, display_query, user_id, session_id, submitted_at
FROM search.query_log
WHERE id > @cursor AND id <= @maxid AND session_id IS NOT NULL
ORDER BY session_id, submitted_at, id;

-- name: ListEngagementSignals :many
-- Click/play signals used to decide whether a query was abandoned, bounded to the
-- sessionizer batch's time window.
SELECT session_id, normalized_query, type, occurred_at
FROM search.behavior_events
WHERE type IN ('search.result_clicked', 'video.play_started')
  AND session_id IS NOT NULL
  AND occurred_at >= @from_ts AND occurred_at <= @to_ts;

-- name: InsertDerivedBehaviorEvent :one
-- Insert one worker-derived behavior event (reformulated/abandoned), deduped on
-- its deterministic event_id. Returns the id only on a fresh insert.
INSERT INTO search.behavior_events
    (event_id, type, user_id, session_id, normalized_query, video_id, occurred_at, props)
VALUES (@event_id, @type, @user_id, @session_id, @normalized_query, @video_id, @occurred_at, @props)
ON CONFLICT (event_id) DO NOTHING
RETURNING event_id;

-- name: DeleteOldQueryLog :execrows
DELETE FROM search.query_log WHERE submitted_at < now() - make_interval(days => @retention_days::int);

-- name: DeleteOldBehaviorEvents :execrows
DELETE FROM search.behavior_events WHERE occurred_at < now() - make_interval(days => @retention_days::int);

-- name: PruneEventsInbox :execrows
DELETE FROM search.events_inbox WHERE received_at < now() - make_interval(days => @days::int);

-- name: PruneWatchProjection :execrows
-- Drop projection rows whose decayed weight has fallen below a floor.
DELETE FROM search.user_watch_projection
WHERE weight * power(2, - GREATEST(0, EXTRACT(EPOCH FROM (now() - last_watched_at)))
                        / (3600.0 * @half_life_hours::double precision)) < @floor::double precision;

-- name: LastReconcileEndAgeSeconds :one
-- Seconds since the most recent reconcile.end was received, or -1 if none is on
-- record (the events_inbox row may have been pruned after 7d — already far past
-- the staleness threshold).
SELECT COALESCE(EXTRACT(EPOCH FROM (now() - max(received_at))), -1)::double precision
FROM search.events_inbox WHERE type = 'reconcile.end';
