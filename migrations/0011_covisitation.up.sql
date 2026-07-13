-- 0011: item-item co-visitation and the derived neighbor index (§1.2, §1.9).
--
-- co_watch / co_search are cumulative, sessionized co-occurrence counters written
-- by the covis_rollup worker: two videos watched (play_started/meaningful_watch)
-- or clicked from the same query within one session, within a time window. The
-- pair is stored in a normalized order (video_a < video_b) so an unordered pair
-- has exactly one row. item_neighbors is the DERIVED, served neighbor index:
-- each pass the worker recomputes shrunk-cosine similarity from the co_* counters
-- and keeps the top-M neighbors per item. Serving a related feed is then one
-- indexed range scan.
CREATE TABLE search.co_watch (
    video_a    UUID NOT NULL,
    video_b    UUID NOT NULL,
    count      BIGINT NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (video_a, video_b),
    CHECK (video_a < video_b)
);

CREATE TABLE search.co_search (
    video_a    UUID NOT NULL,
    video_b    UUID NOT NULL,
    count      BIGINT NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (video_a, video_b),
    CHECK (video_a < video_b)
);

CREATE TABLE search.item_neighbors (
    video_id      UUID NOT NULL,
    neighbor_id   UUID NOT NULL,
    score         REAL NOT NULL,
    source        TEXT NOT NULL CHECK (source IN ('co_watch', 'co_search', 'blend')),
    model_version TEXT NOT NULL,
    PRIMARY KEY (video_id, neighbor_id)
);

-- The related-feed hot path: top neighbors for a seed video, best first.
CREATE INDEX item_neighbors_video_score_idx ON search.item_neighbors (video_id, score DESC);
