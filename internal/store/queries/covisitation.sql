-- Co-visitation worker (§1.9 covis_rollup) and the served neighbor reads.
--
-- AccumulateCoWatch / AccumulateCoSearch are cursor-based, like the other rollup
-- workers: each pass folds behavior_events with id in (cursor, maxid] into the
-- cumulative co-occurrence counters. The pairing counts each unordered pair
-- exactly once: a NEW event is paired only with EARLIER events (p.id < n.id) in
-- the same session (co_watch) or same session+query (co_search) inside the time
-- window. Because every event lands in exactly one cursor range, and the earlier
-- partner is always already durable, the unordered pair {earlier, later} is
-- counted once — when the later event is the anchor — so re-running a rolled-back
-- batch never double counts.

-- name: AccumulateCoWatch :exec
WITH watch AS (
    SELECT id, session_id, video_id, occurred_at
    FROM search.behavior_events
    WHERE type IN ('video.play_started', 'video.meaningful_watch')
      AND session_id IS NOT NULL
      AND video_id IS NOT NULL
),
new_watch AS (
    SELECT id, session_id, video_id, occurred_at FROM watch
    WHERE id > @cursor AND id <= @maxid
),
pairs AS (
    SELECT LEAST(n.video_id, p.video_id)  AS video_a,
           GREATEST(n.video_id, p.video_id) AS video_b,
           count(*)                        AS c
    FROM new_watch n
    JOIN watch p
      ON p.session_id = n.session_id
     AND p.id < n.id
     AND p.video_id <> n.video_id
     AND abs(EXTRACT(EPOCH FROM (n.occurred_at - p.occurred_at))) <= @window_seconds::double precision
    GROUP BY 1, 2
)
INSERT INTO search.co_watch (video_a, video_b, count, updated_at)
SELECT video_a, video_b, c, now() FROM pairs
ON CONFLICT (video_a, video_b) DO UPDATE
    SET count = search.co_watch.count + EXCLUDED.count, updated_at = now();

-- name: AccumulateCoSearch :exec
WITH clk AS (
    SELECT id, session_id, normalized_query, video_id, occurred_at
    FROM search.behavior_events
    WHERE type = 'search.result_clicked'
      AND session_id IS NOT NULL
      AND video_id IS NOT NULL
      AND normalized_query IS NOT NULL
),
new_clk AS (
    SELECT id, session_id, normalized_query, video_id, occurred_at FROM clk
    WHERE id > @cursor AND id <= @maxid
),
pairs AS (
    SELECT LEAST(n.video_id, p.video_id)  AS video_a,
           GREATEST(n.video_id, p.video_id) AS video_b,
           count(*)                        AS c
    FROM new_clk n
    JOIN clk p
      ON p.session_id = n.session_id
     AND p.normalized_query = n.normalized_query
     AND p.id < n.id
     AND p.video_id <> n.video_id
     AND abs(EXTRACT(EPOCH FROM (n.occurred_at - p.occurred_at))) <= @window_seconds::double precision
    GROUP BY 1, 2
)
INSERT INTO search.co_search (video_a, video_b, count, updated_at)
SELECT video_a, video_b, c, now() FROM pairs
ON CONFLICT (video_a, video_b) DO UPDATE
    SET count = search.co_search.count + EXCLUDED.count, updated_at = now();

-- name: ClearCovisNeighbors :exec
-- Step 1 of the neighbor rebuild: drop the covis-v1 index so it can be recomputed
-- from the current co_* counters (both run in the covis worker's transaction).
DELETE FROM search.item_neighbors WHERE model_version = 'covis-v1';

-- name: RebuildCovisNeighbors :exec
-- Step 2 of the neighbor rebuild. For each ordered direction (i → j) the shrunk
-- cosine of a matrix is
--   raw    = cooc(i,j) / sqrt(total(i) * total(j))
--   shrunk = raw * cooc(i,j) / (cooc(i,j) + lambda)
-- where total(i) is i's summed co-occurrence mass in that matrix. The shrinkage
-- factor cooc/(cooc+lambda) damps low-support pairs toward zero (algorithms
-- report λ≈10). co_watch and co_search are blended 0.7 / 0.3, and the top-M
-- neighbors per item (by score, id tie-break) are kept as source='blend'.
WITH cw AS (
    SELECT video_a AS i, video_b AS j, count::double precision AS c FROM search.co_watch
    UNION ALL
    SELECT video_b AS i, video_a AS j, count::double precision AS c FROM search.co_watch
),
cw_tot AS (SELECT i, sum(c) AS t FROM cw GROUP BY i),
cs AS (
    SELECT video_a AS i, video_b AS j, count::double precision AS c FROM search.co_search
    UNION ALL
    SELECT video_b AS i, video_a AS j, count::double precision AS c FROM search.co_search
),
cs_tot AS (SELECT i, sum(c) AS t FROM cs GROUP BY i),
edges AS (
    SELECT COALESCE(cw.i, cs.i) AS i,
           COALESCE(cw.j, cs.j) AS j,
           ( 0.7 * COALESCE(
                 (cw.c / sqrt(cwti.t * cwtj.t)) * (cw.c / (cw.c + @lambda::double precision)), 0)
           + 0.3 * COALESCE(
                 (cs.c / sqrt(csti.t * cstj.t)) * (cs.c / (cs.c + @lambda::double precision)), 0)
           )::real AS score
    FROM cw
    FULL OUTER JOIN cs ON cs.i = cw.i AND cs.j = cw.j
    LEFT JOIN cw_tot cwti ON cwti.i = cw.i
    LEFT JOIN cw_tot cwtj ON cwtj.i = cw.j
    LEFT JOIN cs_tot csti ON csti.i = cs.i
    LEFT JOIN cs_tot cstj ON cstj.i = cs.j
),
ranked AS (
    SELECT i, j, score,
           row_number() OVER (PARTITION BY i ORDER BY score DESC, j) AS rn
    FROM edges
    WHERE score > 0
)
INSERT INTO search.item_neighbors (video_id, neighbor_id, score, source, model_version)
SELECT i, j, score, 'blend', 'covis-v1'
FROM ranked
WHERE rn <= @top_m::int;

-- name: NeighborsForVideo :many
-- Served related candidates for a seed video, best neighbor first.
SELECT neighbor_id, score, source
FROM search.item_neighbors
WHERE video_id = @video_id
ORDER BY score DESC, neighbor_id
LIMIT @lim::int;

-- name: NeighborScoresFromSeeds :many
-- Summed neighbor score of each candidate that neighbors any of the seed videos
-- (session-intent / co-watch candidate scoring). Seeds and candidates are id sets.
SELECT n.neighbor_id AS video_id, sum(n.score)::double precision AS score
FROM search.item_neighbors n
WHERE n.video_id = ANY(@seeds::uuid[])
  AND n.neighbor_id = ANY(@candidates::uuid[])
GROUP BY n.neighbor_id;

-- name: NeighborsForSeeds :many
-- Top neighbor videos of a set of seed videos (advanced home/related candidate
-- generation from session recency), summed score across seeds, excluding seeds.
SELECT n.neighbor_id AS video_id, sum(n.score)::double precision AS score
FROM search.item_neighbors n
WHERE n.video_id = ANY(@seeds::uuid[])
  AND NOT (n.neighbor_id = ANY(@seeds::uuid[]))
GROUP BY n.neighbor_id
ORDER BY score DESC, n.neighbor_id
LIMIT @lim::int;

-- name: NeighborsForUserWatches :many
-- Top neighbor videos of everything the user has watched, weighted by the decayed
-- watch-projection weight (advanced home co-watch candidate generation). Excludes
-- videos the user has already watched.
SELECT n.neighbor_id AS video_id, sum(uwp.weight * n.score)::double precision AS score
FROM search.user_watch_projection uwp
JOIN search.item_neighbors n ON n.video_id = uwp.video_id
WHERE uwp.user_id = @user_id
  AND NOT (n.neighbor_id IN (SELECT video_id FROM search.user_watch_projection WHERE user_id = @user_id))
GROUP BY n.neighbor_id
ORDER BY score DESC, n.neighbor_id
LIMIT @lim::int;
