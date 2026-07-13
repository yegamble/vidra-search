-- 0012: the model registry (§1.2, §1.9).
--
-- One row per trained model artifact. Training happens OFF-line in Python
-- (training/), which writes the artifact under MODEL_DIR and INSERTs a row here
-- with status='shadow'. The Go model_loader worker watches for the kind='ranker'
-- status='active' row and hot-swaps the served model; the shadow-eval worker
-- scores kind='ranker' status='shadow' models against logged impressions and
-- writes NDCG/MRR into metrics. Activation is a manual/admin SQL flip
-- (status active, previous active → retired) — never automatic (§1.9).
--
--   kind:   ranker    — LightGBM LambdaMART text model served via `leaves`.
--           neighbors — materialized item_neighbors refinement (future).
--           factors   — iALS user/item factor matrices (future).
--   status: shadow → active → retired (a model only ever moves forward).
CREATE TABLE search.models (
    id             BIGSERIAL PRIMARY KEY,
    kind           TEXT NOT NULL,
    version        TEXT NOT NULL,
    status         TEXT NOT NULL CHECK (status IN ('shadow', 'active', 'retired')),
    artifact_sha256 TEXT,
    artifact_path  TEXT,
    metrics        JSONB NOT NULL DEFAULT '{}',
    trained_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    activated_at   TIMESTAMPTZ,
    UNIQUE (kind, version)
);

-- The loader/shadow-eval look up the current active/shadow model per kind.
CREATE INDEX models_kind_status_idx ON search.models (kind, status);
