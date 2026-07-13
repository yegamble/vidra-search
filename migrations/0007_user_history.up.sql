-- 0007: per-user personalization projections.
--
-- Both tables are written ONLY from events whose payload carries
-- allow_history=true (vidra-core sets it per instance + user policy). They are
-- purged on user.history_deleted and on user delete, and are the private
-- counterpart to the global query_aggregates.

-- user_search_history: the signed-in user's own recent queries, powering the
-- personal suggestion stream. hidden lets a user remove one entry from
-- suggestions without losing the row's provenance; the per-entry delete endpoint
-- removes the row outright.
CREATE TABLE search.user_search_history (
    user_id          UUID NOT NULL,
    normalized_query TEXT NOT NULL,
    display_query    TEXT NOT NULL DEFAULT '',
    use_count        INT NOT NULL DEFAULT 1,
    last_used_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    hidden           BOOLEAN NOT NULL DEFAULT false,
    PRIMARY KEY (user_id, normalized_query)
);
CREATE INDEX user_search_history_recent_idx
    ON search.user_search_history (user_id, last_used_at DESC);

-- user_watch_projection: decayed per-(user,video) affinity weight, fed by
-- video.play_started (+0.3), derived video.meaningful_watch (+1.0) and
-- video.completed (+1.5). The weight decays with the watch half-life, applied at
-- write time (mathematically equivalent to continuous decay) so no O(N) sweep is
-- needed; the retention worker prunes rows that have decayed below a floor.
CREATE TABLE search.user_watch_projection (
    user_id         UUID NOT NULL,
    video_id        UUID NOT NULL,
    weight          REAL NOT NULL DEFAULT 0,
    last_watched_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, video_id)
);
CREATE INDEX user_watch_projection_user_idx
    ON search.user_watch_projection (user_id, weight DESC);
