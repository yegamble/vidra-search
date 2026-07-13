-- Behavioral event intake (W2). These run inside the same POST /events
-- transaction as the domain effects; the Redis side effects (session context,
-- trending, HLL) are flushed only after that transaction commits.

-- name: InsertBehaviorEvent :exec
-- Durable copy of one behavioral event. event_id is UNIQUE so a redelivery (or a
-- re-derivation with a deterministic id) collapses to one row. props keeps the
-- full original payload for the rollup/sessionizer workers.
INSERT INTO search.behavior_events
    (event_id, type, user_id, session_id, normalized_query, video_id, "position", model_version, occurred_at, props)
VALUES
    (@event_id, @type, @user_id, @session_id, @normalized_query, @video_id, @position, @model_version, @occurred_at, @props)
ON CONFLICT (event_id) DO NOTHING;

-- name: AppendQueryLog :exec
-- One raw query_log row per search.submitted. Deduped on event_id so a replayed
-- batch does not double-count. user_id is retained for exact distinct-user
-- counting (NULLed on history deletion).
INSERT INTO search.query_log
    (event_id, normalized_query, display_query, user_id, session_id, results_count, submitted_at)
VALUES
    (@event_id, @normalized_query, @display_query, @user_id, @session_id, @results_count, @submitted_at)
ON CONFLICT (event_id) DO NOTHING;

-- name: UpsertUserSearchHistory :exec
-- Personal search history — written ONLY when the event allows history. A repeat
-- search bumps use_count and recency; re-searching a previously hidden query
-- un-hides it (it is relevant to the user again).
INSERT INTO search.user_search_history
    (user_id, normalized_query, display_query, use_count, last_used_at, hidden)
VALUES (@user_id, @normalized_query, @display_query, 1, @last_used_at, false)
ON CONFLICT (user_id, normalized_query) DO UPDATE SET
    use_count     = search.user_search_history.use_count + 1,
    display_query = EXCLUDED.display_query,
    last_used_at  = GREATEST(search.user_search_history.last_used_at, EXCLUDED.last_used_at),
    hidden        = false;

-- name: UpsertWatchProjection :exec
-- Decayed per-(user,video) watch affinity — written ONLY when the event allows
-- history. Decay is applied at write time using the stored last_watched_at as the
-- reference (equivalent to continuous decay), so there is no O(N) sweep: an old
-- weight is decayed to now, then the new delta is added.
INSERT INTO search.user_watch_projection (user_id, video_id, weight, last_watched_at)
VALUES (@user_id, @video_id, @delta::real, now())
ON CONFLICT (user_id, video_id) DO UPDATE SET
    weight = search.user_watch_projection.weight
        * power(2, - GREATEST(0, EXTRACT(EPOCH FROM (now() - search.user_watch_projection.last_watched_at)))
                    / (3600.0 * @half_life_hours::double precision))::real
        + EXCLUDED.weight,
    last_watched_at = now();
