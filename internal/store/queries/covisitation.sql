-- Co-visitation worker (§1.9 covis_rollup) and the served neighbor reads.
--
-- The rollup is a FROM-SCRATCH rebuild of the covis-v1 index out of the CURRENTLY
-- RETAINED behavior_events ledger, and nothing else: ClearCovisNeighbors drops the
-- index, then RebuildCovisNeighbors re-pairs the ledger once per source and takes
-- the pair counts, the normalization mass AND the k-anonymity floor's subject
-- counts out of that one pairing. No cursor, no accumulator, one source of truth.
--
-- The pairing counts each unordered pair-instance exactly once: an event is paired
-- only with EARLIER events (p.id < n.id) in the same session (co-watch) or same
-- session+query (co-search) inside the time window, so the unordered pair
-- {earlier, later} is counted once, when the later event is the anchor.
--
-- WHY THIS IS NOT ACCUMULATED. Until this change the counts lived in cumulative
-- search.co_watch / search.co_search tables, folded forward by a cursor over
-- behavior_events. Nothing ever pruned them, so a co-visit kept contributing its
-- co-occurrence to the scores of every surviving pair long after retention deleted
-- both of its events, and a user's purge could not reach it either: the published
-- score was a number no surviving evidence could reproduce. vidra-search#34 had
-- already been forced to recompute the FLOOR from the ledger for exactly that
-- reason — computing the counts in the same CTE is the same pass over the same
-- rows, so respecting retention costs nothing extra here and removes the second
-- store rather than leaving two that can disagree. The counters are RETIRED
-- (migration 0017 marks them on the tables themselves) and are dropped one
-- release later, once no supported release still writes them.

-- name: ClearCovisNeighbors :exec
-- Step 1 of the neighbor rebuild: drop the covis-v1 index so it can be recomputed
-- from the currently retained event ledger (both run in the covis worker's
-- transaction). Dropping first is what lets an edge LEAVE the index — when
-- retention or a purge takes its support below the k-anonymity floor, or takes
-- its co-occurrence down, or when the operator raises that floor — rather than
-- only ever being added to.
DELETE FROM search.item_neighbors WHERE model_version = 'covis-v1';

-- name: RebuildCovisNeighbors :exec
-- Step 2 of the neighbor rebuild. For each ordered direction (i → j) the shrunk
-- cosine of a matrix is
--   raw    = cooc(i,j) / sqrt(total(i) * total(j))
--   shrunk = raw * cooc(i,j) / (cooc(i,j) + lambda)
-- where total(i) is i's summed co-occurrence mass in that matrix. The shrinkage
-- factor cooc/(cooc+lambda) damps low-support pairs toward zero (algorithms
-- report λ≈10). The co-watch and co-search matrices are blended 0.7 / 0.3, and the
-- top-M neighbors per item (by score, id tie-break) are kept as source='blend'.
--
-- EVERY INPUT COMES FROM THE RETAINED LEDGER. cooc(i,j) and total(i) are counted
-- off the same watch_pairs / search_pairs CTEs that count the floor's subjects, so
-- the published score is exactly what recomputing from the events this instance
-- still holds would give. An event that retention deletes, or that a user's purge
-- removes, stops contributing on the next pass — to the score, not only to the
-- floor. That is the whole reason the counts are not accumulated; see the file
-- header for what the cumulative counters used to get wrong.
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
--   1. Counts and support come from ONE pairing of the ledger, not two passes and
--      not a stored counter. A pair-instance is a row of watch_pairs; how many
--      there are is the co-occurrence, how many distinct subjects anchored them is
--      the support. Counting them apart is how the two drifted before: the floor
--      counted people who still exist while the score counted visits that no
--      longer do.
--
--   2. The floor gates PUBLICATION, not counting. cw_mass / cs_mass — the
--      normalization totals — are the full retained mass including below-floor
--      pairs, so a pair at the floor keeps EXACTLY the score it would have without
--      the gate. Filtering the totals too would silently re-scale every surviving
--      neighbour, which is a relevance change wearing a privacy change's clothes.
--
--   3. Each SOURCE is floored independently and its term zeroed, rather than the
--      blended edge being dropped at the end. A below-floor co-search must not
--      publish an edge on its own AND must not inflate one the co-watch half
--      earned. A pair below the floor on both sources scores exactly 0 and is
--      dropped by the `score > 0` that was already there.
--
-- Cost: two self-joins of the retained watch/click ledger per pass (default every
-- 15 min). The work is bounded by session size × retained sessions, NOT by
-- lifetime event volume — pairs only form within one session_id, and the ledger is
-- capped by EVENT_RETENTION_DAYS — so it does not grow with the age of the
-- instance. It is the whole cost of the job now that the cursor pass is gone;
-- docs/operations.md names the symptom and the two knobs.
WITH watch_ev AS (
    SELECT id, session_id, video_id, occurred_at, user_id, props->>'subject_id' AS subject_id
    FROM search.behavior_events
    WHERE type IN ('video.play_started', 'video.meaningful_watch')
      AND session_id IS NOT NULL
      AND video_id IS NOT NULL
),
watch_pairs AS (
    -- One row per co-watched pair: how many co-visits the retained ledger holds
    -- (c) and how many distinct people are behind them (subjects). The subject
    -- count is autosuggest's, byte-for-byte with only the alias changed (see
    -- rollups.sql): distinct account ids, plus distinct anonymous subjects for
    -- rows with no account, preferring core's server-derived subject_id and
    -- falling back to session_id only when the row carries no subject. Identity is
    -- taken from the ANCHOR event n — a pair-instance is one session's visit, and
    -- taking it from one fixed end keeps a session whose two events disagree from
    -- counting twice.
    SELECT LEAST(n.video_id, p.video_id)   AS video_a,
           GREATEST(n.video_id, p.video_id) AS video_b,
           count(*)::double precision       AS c,
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
search_pairs AS (
    SELECT LEAST(n.video_id, p.video_id)   AS video_a,
           GREATEST(n.video_id, p.video_id) AS video_b,
           count(*)::double precision       AS c,
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
    SELECT video_a AS i, video_b AS j, c FROM watch_pairs
    UNION ALL
    SELECT video_b AS i, video_a AS j, c FROM watch_pairs
),
cw_tot AS (SELECT i, sum(c) AS t FROM cw_mass GROUP BY i),
cw_pair AS (
    SELECT video_a, video_b, c FROM watch_pairs WHERE subjects >= @min_subjects::int
),
cw AS (
    SELECT video_a AS i, video_b AS j, c FROM cw_pair
    UNION ALL
    SELECT video_b AS i, video_a AS j, c FROM cw_pair
),
cs_mass AS (
    SELECT video_a AS i, video_b AS j, c FROM search_pairs
    UNION ALL
    SELECT video_b AS i, video_a AS j, c FROM search_pairs
),
cs_tot AS (SELECT i, sum(c) AS t FROM cs_mass GROUP BY i),
cs_pair AS (
    SELECT video_a, video_b, c FROM search_pairs WHERE subjects >= @min_subjects::int
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
