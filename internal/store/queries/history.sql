-- Privacy / history queries (W2). Back the GET/DELETE user history endpoints and
-- the user.history_deleted event handler.
--
-- CLEARING DELETES. It used to anonymize — NULL the user_id and keep the row, so
-- "global aggregates stay intact" — and that was not a weaker version of deletion,
-- it was the opposite of one. Every k-anonymity floor here counts
--
--   count(DISTINCT user_id)
--     + count(DISTINCT CASE WHEN user_id IS NULL THEN COALESCE(subject_id, session_id) END)
--
-- and an ATTRIBUTED row carries no subject_id: core mints the day-scoped
-- anonymous pseudonym only for callers with no account, because beside a known
-- account id it would be redundant and a leak. So NULLing user_id dropped every
-- one of that account's rows through to the client-supplied session fallback, and
-- one person who had used three sessions stopped counting as one subject and
-- started counting as THREE. Measured on a real database: a pair one signed-in
-- user co-watched in three sessions read subjects = 1 before the clear and 3
-- after, which took it over the default floor of 3 and PUBLISHED it into the
-- globally-served "watch this next" index. The same move promoted a
-- below-the-floor private query into instance-wide autosuggest. A privacy action
-- that RAISES a distinct-subject count publishes exactly what the floor exists to
-- suppress, and any single account could trigger it deliberately.
--
-- Deletion is not merely the fix, it is what makes the class of bug impossible:
-- removing rows can only ever lower a distinct-subject count, never raise one.
-- It is also the owner's standing ruling for this area — opt-out means no
-- attributed collection, and a user's contribution must leave the floor AND the
-- scores when their data goes. The aggregates that are recomputed from the
-- ledger (query_aggregates.distinct_users/suggestible, and the whole covis-v1
-- neighbour index, which is a from-scratch rebuild) follow on their next pass.
--
-- Multi-statement operations run in a single transaction so a partial clear can
-- never leave a half-deleted footprint.

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

-- name: ListUserSessionIDs :many
-- The session ids the account's ledger rows carry, read BEFORE the deletes so the
-- ephemeral Redis recency lists keyed on them can be dropped too. Those lists
-- (sess:q:/sess:v:, 2h TTL) are read back into the account's own autosuggest, so
-- leaving them behind means the queries a user just cleared are still offered to
-- them — the one place a cleared query stays user-visible.
SELECT DISTINCT s.session_id::text AS session_id FROM (
    SELECT ql.session_id FROM search.query_log ql
     WHERE ql.user_id = @user_id AND ql.session_id IS NOT NULL
    UNION
    SELECT be.session_id FROM search.behavior_events be
     WHERE be.user_id = @user_id AND be.session_id IS NOT NULL
) s;

-- name: DeleteQueryLogForUser :execrows
DELETE FROM search.query_log WHERE user_id = @user_id;

-- name: DeleteBehaviorEventsForUser :execrows
-- The props clause is a forward guard, not a second copy of the same predicate.
-- Today the user_id COLUMN and props->>'user_id' agree by construction: every
-- branch of applyBehavior maps the decoded payload's UserID into the column. But
-- props stores the FULL original payload, so a new event type whose handler
-- forgets that one field would leave the account id sitting in the JSON with a
-- NULL column, and a column-only delete would miss it silently and for ever.
-- Matching both makes "no row names this account" true of the row rather than of
-- one of its representations. It costs nothing: behavior_events has no index on
-- user_id, so this was already a sequential scan.
DELETE FROM search.behavior_events
WHERE user_id = @user_id OR props->>'user_id' = @user_id::text;

-- name: DeleteQueryLogForUserQuery :execrows
DELETE FROM search.query_log
WHERE user_id = @user_id AND normalized_query = @normalized_query;

-- name: DeleteBehaviorEventsForUserQuery :execrows
-- The per-entry delete's ledger half. "Forget I searched X" and "forget
-- everything" have to mean the same kind of thing: if clear-all deletes, leaving
-- the account's rows for that one query behind would keep them counted toward X's
-- instance-wide distinct-subject floor after the user asked to be forgotten for
-- X. Scoped to rows carrying that query, so a watch with no search context is
-- untouched — and the durable watch projection, which is keyed (user, video) with
-- no query, is not in scope for a search-history action at all.
DELETE FROM search.behavior_events
WHERE (user_id = @user_id OR props->>'user_id' = @user_id::text)
  AND normalized_query = @normalized_query;
