-- Down: drop the deletion indexes and restore the comments 0006 / 0009 implied
-- (which is no comment at all -- COMMENT ... IS NULL removes one).
DROP INDEX IF EXISTS search.behavior_events_user_id_idx;
DROP INDEX IF EXISTS search.behavior_events_props_user_id_idx;
DROP INDEX IF EXISTS search.query_log_user_id_idx;
COMMENT ON TABLE search.query_video_engagement IS NULL;
COMMENT ON COLUMN search.query_aggregates.total_count IS NULL;
COMMENT ON COLUMN search.query_aggregates.decayed_freq IS NULL;
