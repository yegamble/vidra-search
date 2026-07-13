-- name: UpsertServiceConfig :exec
INSERT INTO search.service_config (key, value, updated_at)
VALUES (@key, @value, now())
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now();

-- name: ListServiceConfig :many
SELECT key, value, updated_at FROM search.service_config ORDER BY key;
