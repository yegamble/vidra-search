-- Experiment definitions (§1.5). The experiment package caches enabled rows in
-- RAM and refreshes them periodically; assignment is a pure hash bucketing.

-- name: ListEnabledExperiments :many
SELECT key, description, variants, salt, enabled, created_at
FROM search.experiments
WHERE enabled
ORDER BY key;

-- name: UpsertExperiment :exec
-- Define or update an experiment (admin/seed; used by tests).
INSERT INTO search.experiments (key, description, variants, salt, enabled)
VALUES (@key, @description, @variants, @salt, @enabled)
ON CONFLICT (key) DO UPDATE
    SET description = EXCLUDED.description,
        variants = EXCLUDED.variants,
        salt = EXCLUDED.salt,
        enabled = EXCLUDED.enabled;
