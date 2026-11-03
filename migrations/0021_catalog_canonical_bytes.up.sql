-- Charge exact canonical documents-array bytes at page admission. Compressed
-- JSONB storage size cannot establish the agreed wire budget.
ALTER TABLE search.catalog_snapshots
    ADD COLUMN canonical_bytes BIGINT NOT NULL DEFAULT 0
    CHECK (canonical_bytes >= 0 AND canonical_bytes <= 536870912);

COMMENT ON COLUMN search.catalog_snapshots.canonical_bytes IS
    'Sum of canonical documents JSON array byte lengths for distinct pending pages; duplicates add zero.';
