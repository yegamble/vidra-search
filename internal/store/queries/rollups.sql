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
-- Fold new query_log rows (id in (cursor, maxid]) into query_aggregates.
-- decayed_freq is decay-then-increment (half-life from config); distinct_users is
-- an EXACT recount over the retained window (distinct user_id + session fallback);
-- display_query is the most recent display form; suggestible clears the min
-- distinct-user threshold and is never true for a banned query.
WITH batch AS (
    SELECT ql.normalized_query,
           count(*)             AS delta,
           min(ql.submitted_at) AS batch_first_seen,
           max(ql.submitted_at) AS batch_last_seen
    FROM search.query_log ql
    WHERE ql.id > @from_id AND ql.id <= @maxid AND ql.normalized_query <> ''
    GROUP BY ql.normalized_query
),
recount AS (
    -- Exact distinct users: count(DISTINCT user_id) ignores NULLs automatically;
    -- anonymous rows fall back to their distinct session_id. FILTER is avoided
    -- deliberately (it trips sqlc 1.31.1's named-parameter editor here).
    SELECT b.normalized_query,
           (count(DISTINCT ql.user_id)
              + count(DISTINCT CASE WHEN ql.user_id IS NULL THEN ql.session_id END))::int AS distinct_users
    FROM batch b
    JOIN search.query_log ql ON ql.normalized_query = b.normalized_query
        AND ql.submitted_at >= @window_start
    GROUP BY b.normalized_query
),
display AS (
    SELECT DISTINCT ON (ql.normalized_query) ql.normalized_query, ql.display_query
    FROM search.query_log ql
    JOIN batch b ON b.normalized_query = ql.normalized_query
    ORDER BY ql.normalized_query, ql.submitted_at DESC, ql.id DESC
)
-- The full new row (including the decayed_freq computed from the LEFT-JOINed
-- existing aggregate) is built in the SELECT, so the ON CONFLICT clause is a
-- trivial column-wise overwrite. The aggregates_rollup worker is a single writer
-- inside one transaction, so the read-compute-upsert is race-free.
INSERT INTO search.query_aggregates
    (normalized_query, display_query, total_count, distinct_users, decayed_freq, first_seen, last_seen, suggestible, banned)
SELECT b.normalized_query,
       d.display_query,
       COALESCE(qa.total_count, 0) + b.delta,
       r.distinct_users,
       COALESCE(qa.decayed_freq, 0)
           * power(2, - (CASE WHEN qa.last_seen IS NULL THEN 0
                              ELSE GREATEST(0, EXTRACT(EPOCH FROM (b.batch_last_seen - qa.last_seen))) END)
                        / @half_life_seconds::double precision)
           + b.delta::double precision,
       LEAST(qa.first_seen, b.batch_first_seen),
       GREATEST(qa.last_seen, b.batch_last_seen),
       (r.distinct_users >= @min_users::int) AND NOT COALESCE(qa.banned, false),
       COALESCE(qa.banned, false)
FROM batch b
JOIN recount r ON r.normalized_query = b.normalized_query
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
    jsonb_build_object('allow_history', COALESCE((wp.props->>'allow_history')::boolean, false),
                       'derived_from', 'watch_progress')
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

-- name: FoldEngagement :exec
-- Fold new behavior_events (id in (cursor, maxid]) into per-(query,video)
-- engagement counters. Meaningful-watch rows derived in the current pass have
-- id > maxid, so they are folded on the next pass — counted exactly once.
INSERT INTO search.query_video_engagement
    (normalized_query, video_id, impressions, clicks, meaningful_watches, updated_at)
SELECT be.normalized_query, be.video_id,
       count(*) FILTER (WHERE be.type = 'video.impression'),
       count(*) FILTER (WHERE be.type = 'search.result_clicked'),
       count(*) FILTER (WHERE be.type = 'video.meaningful_watch'),
       now()
FROM search.behavior_events be
WHERE be.id > @cursor AND be.id <= @maxid
  AND be.normalized_query IS NOT NULL
  AND be.video_id IS NOT NULL
  AND be.type IN ('video.impression', 'search.result_clicked', 'video.meaningful_watch')
GROUP BY be.normalized_query, be.video_id
ON CONFLICT (normalized_query, video_id) DO UPDATE SET
    impressions        = search.query_video_engagement.impressions + EXCLUDED.impressions,
    clicks             = search.query_video_engagement.clicks + EXCLUDED.clicks,
    meaningful_watches = search.query_video_engagement.meaningful_watches + EXCLUDED.meaningful_watches,
    updated_at         = now();

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
