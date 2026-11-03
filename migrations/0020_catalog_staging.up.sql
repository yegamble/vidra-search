-- Only the newest pending snapshot is retained per ownership scope. Payload
-- cleanup cascades independently of the permanent epoch/completion authority.
ALTER TABLE search.catalog_scopes
    ADD CONSTRAINT catalog_scopes_scope_epoch_key UNIQUE (scope, epoch);

CREATE TABLE search.catalog_snapshots (
    scope TEXT PRIMARY KEY,
    epoch UUID NOT NULL,
    generation BIGINT NOT NULL CHECK (generation > 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    page_count INTEGER CHECK (page_count >= 0),
    document_count BIGINT CHECK (document_count >= 0),
    membership_sha256 TEXT CHECK (membership_sha256 ~ '^[0-9a-f]{64}$'),
    pages_sha256 TEXT CHECK (pages_sha256 ~ '^[0-9a-f]{64}$'),
    FOREIGN KEY (scope, epoch) REFERENCES search.catalog_scopes(scope, epoch) ON DELETE CASCADE,
    UNIQUE (scope, epoch, generation),
    CHECK (page_count <= document_count),
    CHECK ((page_count = 0) = (document_count = 0)),
    CHECK (
        (page_count IS NULL AND document_count IS NULL
            AND membership_sha256 IS NULL AND pages_sha256 IS NULL)
        OR
        (page_count IS NOT NULL AND document_count IS NOT NULL
            AND membership_sha256 IS NOT NULL AND pages_sha256 IS NOT NULL)
    )
);

CREATE TABLE search.catalog_snapshot_pages (
    scope TEXT NOT NULL,
    epoch UUID NOT NULL,
    generation BIGINT NOT NULL,
    page_index INTEGER NOT NULL CHECK (page_index >= 0),
    documents JSONB NOT NULL CHECK (jsonb_typeof(documents) = 'array' AND jsonb_array_length(documents) > 0),
    document_count INTEGER NOT NULL CHECK (document_count > 0 AND document_count = jsonb_array_length(documents)),
    content_sha256 TEXT NOT NULL CHECK (content_sha256 ~ '^[0-9a-f]{64}$'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (scope, epoch, generation, page_index),
    FOREIGN KEY (scope, epoch, generation)
        REFERENCES search.catalog_snapshots(scope, epoch, generation) ON DELETE CASCADE
);
