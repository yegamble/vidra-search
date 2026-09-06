-- 0018: make "Clear all" index-driven, and say on the tables what the aggregates
-- now mean.
--
-- PART 1 — the deletion indexes.
--
-- vidra-search#37 made the shipped clear-all DELETE the caller's rows instead of
-- NULLing user_id, and noted that neither ledger has an index on user_id: every
-- clear-all was a sequential scan of the whole retention window. That was true
-- before the change too (the UPDATE scanned just as hard), but a privacy control
-- whose cost is proportional to the instance's total traffic is one an operator
-- learns to dread, and it is the one control a user is most entitled to press.
--
-- Measured on a throwaway postgres:18-alpine with 400k behavior_events / 300k
-- query_log rows over 20k accounts (EXPLAIN ANALYZE of the shipped predicates,
-- inside a rolled-back transaction):
--
--   behavior_events clear-all: Seq Scan, 10046 buffers, 43.4 ms
--                        ->    BitmapOr of the two indexes below, 36 buffers, 0.18 ms
--   query_log clear-all:       Seq Scan,  4285 buffers, 12.0 ms
--                        ->    Bitmap Index Scan, 33 buffers, 0.08 ms
--
-- BOTH behavior_events indexes are needed, and that is the non-obvious part.
-- DeleteBehaviorEventsForUser matches `user_id = $1 OR props->>'user_id' = $1`
-- (the props half is #37's forward guard against a future event type whose
-- handler forgets to map the column). An OR can only go through indexes when
-- BOTH sides have one — with the column index alone, Postgres re-plans the whole
-- statement as a Seq Scan and the index is never touched (verified by dropping
-- the props index: back to 10046 buffers). So the second index is not belt and
-- braces; it is what makes the first one reachable.
--
-- All three are PARTIAL, on `... IS NOT NULL`. Anonymous rows carry no account id
-- in either representation and can never match the predicate, so indexing them
-- would be pure write amplification on the hottest insert path in the service.
-- Postgres proves the partial predicate from the equality (a strict operator
-- implies IS NOT NULL on its argument), so the partial index is usable by the
-- shipped queries as written. On the fixture above they cost 2744 kB + 3248 kB +
-- 2744 kB, against 34 MB for the existing behavior_events_type_occurred_idx.
--
-- NOT `CONCURRENTLY`, deliberately, matching every other index in this schema.
-- It does work through the embedded migrator (golang-migrate's postgres driver
-- does not wrap a migration in an explicit transaction — verified here), and it
-- would avoid the write lock a plain build takes for its duration. It is still
-- the wrong trade for a migration that deploy.sh runs as an exit-code-gated step
-- and aborts on: a failed CONCURRENTLY build leaves an INVALID index behind that
-- nothing uses and nothing reports, and with three statements in one file and no
-- transaction, a failure on the second leaves the first committed. A plain build
-- fails cleanly and is retried by re-running the step. The cost is a SHARE lock
-- (writers blocked, readers not) for the build: 586 ms / 274 ms / 82 ms on the
-- fixture above. An operator whose ledger is large enough for that to matter has
-- a soft dependency — core falls back on its own SQL and the outbox drainer
-- retries — and can always build these by hand CONCURRENTLY before deploying.
CREATE INDEX behavior_events_user_id_idx
    ON search.behavior_events (user_id) WHERE user_id IS NOT NULL;
CREATE INDEX behavior_events_props_user_id_idx
    ON search.behavior_events ((props->>'user_id')) WHERE props->>'user_id' IS NOT NULL;
CREATE INDEX query_log_user_id_idx
    ON search.query_log (user_id) WHERE user_id IS NOT NULL;

-- PART 2 — the comments, for the same reason 0017 commented the retired counters:
-- a table whose creating migration describes it as something it is no longer is a
-- trap for the next reader, and the creating migration cannot be edited.
--
-- 0009 says query_video_engagement is folded forward by a cursor. It is not any
-- more: it is cleared and rebuilt from the retained behavior_events every pass,
-- because a cumulative counter outlives the events behind it and neither
-- retention nor a user's deletion could reach it — the same defect vidra-search#36
-- removed from the co-visitation counters, and the same ruling.
COMMENT ON TABLE search.query_video_engagement IS
    'Per-(query, video) engagement counters, RECOMPUTED from the retained search.behavior_events on every engagement_rollup pass (clear + rebuild, no cursor). NOT cumulative -- migration 0009''s description of a cursor fold is superseded. Read live: SearchAdvancedRecall recalls candidates on clicks > 0 and feeds impressions/clicks/meaningful_watches into the stage-2 CTR and meaningful-watch-rate features, so a counter that outlived its evidence kept a deleted click ranking a video.';

COMMENT ON COLUMN search.query_aggregates.total_count IS
    'Searches for this query the instance STILL HOLDS inside EVENT_RETENTION_DAYS, recomputed from search.query_log by the rollup (queries with new traffic) and by suggestible_reeval (every row, daily). Not cumulative -- migration 0006''s decay-then-increment description is superseded.';
COMMENT ON COLUMN search.query_aggregates.decayed_freq IS
    'Recency-weighted popularity and the ORDER of the aggregate autosuggest stream: sum over the RETAINED query_log rows of 2^(-age/half-life), anchored on this query''s own last_seen, at SEARCH_QUERY_HALF_LIFE_HOURS. Recomputed, not accumulated, so searches this instance has deleted cannot keep buying a completion its rank.';
