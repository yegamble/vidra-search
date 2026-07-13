-- 0005: the raw query log.
--
-- One row per search.submitted behavioral event. This is the durable stream the
-- aggregates_rollup worker folds into query_aggregates (global suggestible
-- queries) and the sessionizer reads to derive reformulated/abandoned events.
-- normalized_query is the folded key (matched against the corpus); display_query
-- preserves the user's typed form for surfacing. user_id is retained for exact
-- distinct-user counting and is NULLed (anonymized) on history deletion.
CREATE TABLE search.query_log (
    id               BIGSERIAL PRIMARY KEY,
    event_id         UUID UNIQUE,
    normalized_query TEXT NOT NULL,
    display_query    TEXT NOT NULL DEFAULT '',
    user_id          UUID,
    session_id       TEXT,
    results_count    INT,
    submitted_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Rollup + sessionizer scan by recency; retention deletes by submitted_at.
CREATE INDEX query_log_submitted_at_idx ON search.query_log (submitted_at);
-- Distinct-user recount + display-form lookup filter by normalized_query.
CREATE INDEX query_log_normalized_query_idx ON search.query_log (normalized_query);
-- Sessionizer groups by session over a time window.
CREATE INDEX query_log_session_idx ON search.query_log (session_id, submitted_at);
