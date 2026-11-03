-- Canonical catalog authority is independent of the expiring event inbox.
-- Empty tables keep this receiver foundation dormant until explicit bootstrap.
CREATE TABLE search.catalog_epochs (
    scope TEXT NOT NULL CHECK (scope = 'local'),
    epoch UUID NOT NULL CHECK (epoch <> '00000000-0000-0000-0000-000000000000'),
    predecessor UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (scope, epoch),
    FOREIGN KEY (scope, predecessor) REFERENCES search.catalog_epochs(scope, epoch),
    CHECK (predecessor IS NULL OR predecessor <> epoch)
);

CREATE TABLE search.catalog_scopes (
    scope TEXT PRIMARY KEY CHECK (scope = 'local'),
    epoch UUID NOT NULL,
    highest_observed_generation BIGINT NOT NULL DEFAULT 0
        CHECK (highest_observed_generation >= 0),
    completed_generation BIGINT NOT NULL DEFAULT 0
        CHECK (completed_generation >= 0 AND completed_generation <= highest_observed_generation),
    completed_at TIMESTAMPTZ,
    completed_page_count INTEGER NOT NULL DEFAULT 0 CHECK (completed_page_count >= 0),
    completed_document_count BIGINT NOT NULL DEFAULT 0 CHECK (completed_document_count >= 0),
    completed_membership_sha256 TEXT CHECK (completed_membership_sha256 ~ '^[0-9a-f]{64}$'),
    completed_pages_sha256 TEXT CHECK (completed_pages_sha256 ~ '^[0-9a-f]{64}$'),
    FOREIGN KEY (scope, epoch) REFERENCES search.catalog_epochs(scope, epoch),
    CHECK (completed_page_count <= completed_document_count),
    CHECK ((completed_page_count = 0) = (completed_document_count = 0)),
    CHECK (
        (completed_generation = 0 AND completed_at IS NULL
            AND completed_page_count = 0 AND completed_document_count = 0
            AND completed_membership_sha256 IS NULL AND completed_pages_sha256 IS NULL)
        OR
        (completed_generation > 0 AND completed_at IS NOT NULL
            AND completed_membership_sha256 IS NOT NULL AND completed_pages_sha256 IS NOT NULL)
    )
);

COMMENT ON TABLE search.catalog_epochs IS
    'Permanent epoch/predecessor history; never pruned with inbox or staged payloads.';
COMMENT ON COLUMN search.catalog_scopes.highest_observed_generation IS
    'Admission/expiry fence only. Staging cannot establish catalog completeness.';
COMMENT ON TABLE search.catalog_scopes IS
    'Permanent completed-full authority and manifest. Restore must preserve producer and receiver state consistently.';
