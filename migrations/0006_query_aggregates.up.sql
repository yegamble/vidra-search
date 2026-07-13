-- 0006: rolled-up global query popularity.
--
-- The aggregates_rollup worker folds query_log into this table. decayed_freq is
-- the recency-weighted frequency (decay-then-increment, half-life from config);
-- distinct_users is an EXACT recount over the retained query_log window
-- (COUNT(DISTINCT user_id) + session fallback for anonymous) — chosen over an HLL
-- estimate because it is simpler and honest at this scale. A query is suggestible
-- only when its distinct-user count clears the min threshold AND it is not banned,
-- so a single user spamming one query can never become a global suggestion.
CREATE TABLE search.query_aggregates (
    normalized_query TEXT PRIMARY KEY,
    display_query    TEXT NOT NULL DEFAULT '',
    total_count      BIGINT NOT NULL DEFAULT 0,
    distinct_users   INT NOT NULL DEFAULT 0,
    decayed_freq     DOUBLE PRECISION NOT NULL DEFAULT 0,
    first_seen       TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen        TIMESTAMPTZ NOT NULL DEFAULT now(),
    suggestible      BOOLEAN NOT NULL DEFAULT false,
    banned           BOOLEAN NOT NULL DEFAULT false
);

-- Anchored prefix completion for the aggregate suggestion stream (LIKE 'p%').
-- The PK's default btree opclass cannot drive LIKE under a non-C locale, so a
-- dedicated text_pattern_ops index makes suggestible-query prefix scans index-driven.
CREATE INDEX query_aggregates_nq_prefix_idx
    ON search.query_aggregates (normalized_query text_pattern_ops);
-- Suggestion stream orders suggestible rows by decayed_freq.
CREATE INDEX query_aggregates_suggestible_freq_idx
    ON search.query_aggregates (decayed_freq DESC) WHERE suggestible AND NOT banned;
