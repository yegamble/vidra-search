-- 0013: experiment definitions (§1.2, §1.5).
--
-- A deterministic bucketing table: for an enabled experiment, a subject is
-- assigned to a variant by hash(salt + subject_id) % 100 falling in that
-- variant's [min,max) bucket range. Definitions are cached in RAM by the
-- experiment package and refreshed periodically. The chosen variant is stamped
-- into search/recommendation responses (experiment field) and, via model_version,
-- into the impression log — so an offline analysis can attribute outcomes to a
-- variant without any server-side per-request event write.
--
-- variants JSONB shape:
--   [{"name": "control", "min": 0, "max": 50, "model_version": "heuristic-v1"},
--    {"name": "learned", "min": 50, "max": 100, "model_version": "ranker-v1"}]
CREATE TABLE search.experiments (
    key         TEXT PRIMARY KEY,
    description TEXT NOT NULL DEFAULT '',
    variants    JSONB NOT NULL DEFAULT '[]',
    salt        TEXT NOT NULL DEFAULT '',
    enabled     BOOLEAN NOT NULL DEFAULT false,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
