-- 0004: policy configuration pushed from vidra-core.
--
-- A small key/value overlay carrying the search-relevant instance settings
-- (search_mode, suggestions_enabled, minimum_query_user_count, retention days,
-- trending half-life, ...). vidra-core is authoritative and pushes changes via
-- the search.config_updated event; the service reads these to override its env
-- defaults at runtime. Never holds secrets.
CREATE TABLE search.service_config (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
