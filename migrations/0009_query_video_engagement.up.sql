-- 0009: per-(query, video) engagement counters.
--
-- The engagement_rollup worker folds behavior_events into this table:
-- impressions (video.impression with a query in context), clicks
-- (search.result_clicked) and meaningful_watches (derived video.meaningful_watch).
-- It powers de-biased CTR and meaningful-watch-rate features for advanced search
-- ranking (W3) and is a small, bounded aggregate rather than a per-event log.
CREATE TABLE search.query_video_engagement (
    normalized_query   TEXT NOT NULL,
    video_id           UUID NOT NULL,
    impressions        BIGINT NOT NULL DEFAULT 0,
    clicks             BIGINT NOT NULL DEFAULT 0,
    meaningful_watches BIGINT NOT NULL DEFAULT 0,
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (normalized_query, video_id)
);
CREATE INDEX query_video_engagement_video_idx ON search.query_video_engagement (video_id);
