-- Privacy / history queries (W2). Back the GET/DELETE user history endpoints and
-- the user.history_deleted event handler. Deleting personal history NULLs the
-- user_id in the raw logs (anonymization) rather than deleting the rows, so global
-- aggregates stay intact while the data no longer references the user.

-- name: ListUserSearchHistory :many
SELECT normalized_query, display_query, use_count, last_used_at
FROM search.user_search_history
WHERE user_id = @user_id AND NOT hidden
ORDER BY last_used_at DESC
LIMIT @lim::int OFFSET @off::int;

-- name: DeleteUserSearchHistory :exec
DELETE FROM search.user_search_history WHERE user_id = @user_id;

-- name: DeleteUserSearchHistoryEntry :exec
DELETE FROM search.user_search_history
WHERE user_id = @user_id AND normalized_query = @normalized_query;

-- name: PurgeUserWatchProjection :exec
DELETE FROM search.user_watch_projection WHERE user_id = @user_id;

-- name: AnonymizeQueryLogUser :exec
UPDATE search.query_log SET user_id = NULL WHERE user_id = @user_id;

-- name: AnonymizeBehaviorEventsUser :exec
UPDATE search.behavior_events SET user_id = NULL WHERE user_id = @user_id;
