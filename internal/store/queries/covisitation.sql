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
-- from the current co_* counters AND the currently retained event ledger (both
-- run in the covis worker's transaction). Dropping first is what lets an edge
-- LEAVE the index — when retention takes its support below the k-anonymity floor,
-- or when the operator raises that floor — rather than only ever being added to.
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
--
-- THE K-ANONYMITY FLOOR. A pair is published only once at least @min_subjects
-- DISTINCT SUBJECTS co-visited it. Shrinkage alone is a ranking device, not a
-- privacy one: it ranks a one-person pair low, it still publishes it, and on a
-- quiet instance low is first. item_neighbors is a globally-served index, so one
-- person opening C1 then P then U in one session used to publish three
-- associations to every viewer. Autosuggest gates a query behind
-- `minimum_query_user_count` and trending gates a video behind the same floor;
-- the same argument applies here verbatim, so this reuses that knob rather than
-- adding a parallel one — an operator who says "three people before you publish
-- something about me" means it about all three surfaces.
--
-- Three deliberate choices, because each has a cheaper-looking alternative:
--
--   1. Support is recomputed FROM THE EVENT LEDGER, not read off co_watch /
--      co_search. Those counters hold visits, not people: six co-visits by one
--      subject is count=6. They are also cumulative and never pruned, so support
--      read out of them would outlive the evidence retention deleted. Reading
--      behavior_events makes the floor a live question every pass — which is what
--      keeps this a from-scratch rebuild (ClearCovisNeighbors runs first), so an
--      edge whose support ages out disappears on the next rollup.
--
--   2. The floor gates PUBLICATION, not counting. cw_mass / cs_mass — the
--      normalization totals — are still the full counters, so a pair at the floor
--      keeps EXACTLY the score it had before this gate existed. Filtering the
--      totals too would silently re-scale every surviving neighbour, which is a
--      relevance change wearing a privacy change's clothes.
--
--   3. Each SOURCE is floored independently and its term zeroed, rather than the
--      blended edge being dropped at the end. A below-floor co-search must not
--      publish an edge on its own AND must not inflate one the co-watch half
--      earned. A pair below the floor on both sources scores exactly 0 and is
--      dropped by the `score > 0` that was already there.
--
-- Cost: the two support CTEs re-pair the retained watch/click ledger every pass
-- (default every 15 min), where the accumulators above only pair the new cursor
-- slice against it. The work is bounded by session size, not table size — pairs
-- only form within one session_id — but it is the expensive half of this job now.
WITH watch_ev AS (
    SELECT id, session_id, video_id, occurred_at, user_id, props->>'subject_id' AS subject_id
    FROM search.behavior_events
    WHERE type IN ('video.play_started', 'video.meaningful_watch')
      AND session_id IS NOT NULL
      AND video_id IS NOT NULL
),
watch_support AS (
    -- Distinct subjects behind each co-watched pair. The count is autosuggest's,
    -- byte-for-byte with only the alias changed (see rollups.sql): distinct
    -- account ids, plus distinct anonymous subjects for rows with no account,
    -- preferring core's server-derived subject_id and falling back to session_id
    -- only when the row carries no subject. Identity is taken from the ANCHOR
    -- event n — a pair-instance is one session's visit, and taking it from one
    -- fixed end keeps a session whose two events disagree from counting twice.
    SELECT LEAST(n.video_id, p.video_id)   AS video_a,
           GREATEST(n.video_id, p.video_id) AS video_b,
           (count(DISTINCT n.user_id)
              + count(DISTINCT CASE WHEN n.user_id IS NULL THEN COALESCE(n.subject_id, n.session_id) END))::int AS subjects
    FROM watch_ev n
    JOIN watch_ev p
      ON p.session_id = n.session_id
     AND p.id < n.id
     AND p.video_id <> n.video_id
     AND abs(EXTRACT(EPOCH FROM (n.occurred_at - p.occurred_at))) <= @window_seconds::double precision
    GROUP BY 1, 2
),
search_ev AS (
    SELECT id, session_id, normalized_query, video_id, occurred_at, user_id, props->>'subject_id' AS subject_id
    FROM search.behavior_events
    WHERE type = 'search.result_clicked'
      AND session_id IS NOT NULL
      AND video_id IS NOT NULL
      AND normalized_query IS NOT NULL
),
search_support AS (
    SELECT LEAST(n.video_id, p.video_id)   AS video_a,
           GREATEST(n.video_id, p.video_id) AS video_b,
           (count(DISTINCT n.user_id)
              + count(DISTINCT CASE WHEN n.user_id IS NULL THEN COALESCE(n.subject_id, n.session_id) END))::int AS subjects
    FROM search_ev n
    JOIN search_ev p
      ON p.session_id = n.session_id
     AND p.normalized_query = n.normalized_query
     AND p.id < n.id
     AND p.video_id <> n.video_id
     AND abs(EXTRACT(EPOCH FROM (n.occurred_at - p.occurred_at))) <= @window_seconds::double precision
    GROUP BY 1, 2
),
cw_mass AS (
    SELECT video_a AS i, video_b AS j, count::double precision AS c FROM search.co_watch
    UNION ALL
    SELECT video_b AS i, video_a AS j, count::double precision AS c FROM search.co_watch
),
cw_tot AS (SELECT i, sum(c) AS t FROM cw_mass GROUP BY i),
cw_pair AS (
    SELECT w.video_a, w.video_b, w.count::double precision AS c
    FROM search.co_watch w
    JOIN watch_support s ON s.video_a = w.video_a AND s.video_b = w.video_b
    WHERE s.subjects >= @min_subjects::int
),
cw AS (
    SELECT video_a AS i, video_b AS j, c FROM cw_pair
    UNION ALL
    SELECT video_b AS i, video_a AS j, c FROM cw_pair
),
cs_mass AS (
    SELECT video_a AS i, video_b AS j, count::double precision AS c FROM search.co_search
    UNION ALL
    SELECT video_b AS i, video_a AS j, count::double precision AS c FROM search.co_search
),
cs_tot AS (SELECT i, sum(c) AS t FROM cs_mass GROUP BY i),
cs_pair AS (
    SELECT k.video_a, k.video_b, k.count::double precision AS c
    FROM search.co_search k
    JOIN search_support s ON s.video_a = k.video_a AND s.video_b = k.video_b
    WHERE s.subjects >= @min_subjects::int
),
cs AS (
    SELECT video_a AS i, video_b AS j, c FROM cs_pair
    UNION ALL
    SELECT video_b AS i, video_a AS j, c FROM cs_pair
),
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
