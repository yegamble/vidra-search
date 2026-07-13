-- Shadow evaluation (§1.9). Replays the last N days of logged impressions with
-- their click / meaningful-watch labels so a shadow ranker can be scored offline
-- (NDCG@10 / MRR@10) against the production ordering actually served and against
-- a heuristic re-rank — with NO effect on live serving.

-- name: ShadowImpressions :many
-- One row per logged impression (video.impression) in the window, carrying the
-- served position and whether that (session, query, video) was later clicked
-- and/or meaningful-watched. Label derivation (documented in docs/evaluation.md):
-- meaningful-watch ⇒ graded relevance 2, click ⇒ 1, neither ⇒ 0. Grouped by
-- (session, query) into ranked impression lists in Go.
SELECT
    imp.session_id,
    imp.normalized_query,
    imp.video_id,
    COALESCE(imp.position, 0)::int AS position,
    imp.occurred_at,
    EXISTS (
        SELECT 1 FROM search.behavior_events c
        WHERE c.type = 'search.result_clicked'
          AND c.session_id IS NOT DISTINCT FROM imp.session_id
          AND c.normalized_query = imp.normalized_query
          AND c.video_id = imp.video_id
          AND c.occurred_at >= imp.occurred_at
    ) AS clicked,
    EXISTS (
        SELECT 1 FROM search.behavior_events m
        WHERE m.type = 'video.meaningful_watch'
          AND m.session_id IS NOT DISTINCT FROM imp.session_id
          AND m.video_id = imp.video_id
          AND m.occurred_at >= imp.occurred_at
    ) AS meaningful
FROM search.behavior_events imp
WHERE imp.type = 'video.impression'
  AND imp.normalized_query IS NOT NULL
  AND imp.video_id IS NOT NULL
  AND imp.occurred_at >= now() - make_interval(days => @days::int)
ORDER BY imp.session_id NULLS FIRST, imp.normalized_query, position, imp.video_id;
