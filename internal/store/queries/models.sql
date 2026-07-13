-- Model registry (§1.9). Python training INSERTs shadow rows; the Go model_loader
-- reads the active ranker, the shadow-eval worker reads shadow rankers and writes
-- metrics, and activation flips status (retire previous, activate target).

-- name: GetActiveModel :one
-- The currently-served model of a kind, if any.
SELECT id, kind, version, status, artifact_sha256, artifact_path, metrics, trained_at, activated_at
FROM search.models
WHERE kind = @kind AND status = 'active'
ORDER BY activated_at DESC NULLS LAST, id DESC
LIMIT 1;

-- name: ListShadowModels :many
-- Shadow models of a kind awaiting evaluation, newest first.
SELECT id, kind, version, status, artifact_sha256, artifact_path, metrics, trained_at, activated_at
FROM search.models
WHERE kind = @kind AND status = 'shadow'
ORDER BY trained_at DESC, id DESC;

-- name: GetModelByVersion :one
SELECT id, kind, version, status, artifact_sha256, artifact_path, metrics, trained_at, activated_at
FROM search.models
WHERE kind = @kind AND version = @version;

-- name: ListModels :many
SELECT id, kind, version, status, artifact_sha256, artifact_path, metrics, trained_at, activated_at
FROM search.models
ORDER BY kind, trained_at DESC, id DESC;

-- name: InsertModel :one
-- Register a freshly-trained artifact (used by Go tests; Python training inserts
-- the same shape directly via psycopg).
INSERT INTO search.models (kind, version, status, artifact_sha256, artifact_path, metrics, trained_at)
VALUES (@kind, @version, @status, @artifact_sha256, @artifact_path, @metrics, now())
ON CONFLICT (kind, version) DO UPDATE
    SET status = EXCLUDED.status,
        artifact_sha256 = EXCLUDED.artifact_sha256,
        artifact_path = EXCLUDED.artifact_path,
        metrics = EXCLUDED.metrics,
        trained_at = now()
RETURNING id;

-- name: UpdateModelMetrics :exec
-- Record shadow-evaluation metrics onto a model row.
UPDATE search.models SET metrics = @metrics WHERE id = @id;

-- name: RetireActiveModels :exec
-- Retire the currently-active model(s) of a kind (activation step 1).
UPDATE search.models
SET status = 'retired'
WHERE kind = @kind AND status = 'active';

-- name: ActivateModel :exec
-- Promote a specific version to active (activation step 2).
UPDATE search.models
SET status = 'active', activated_at = now()
WHERE kind = @kind AND version = @version;
