DROP TABLE IF EXISTS search.catalog_snapshot_pages;
DROP TABLE IF EXISTS search.catalog_snapshots;
ALTER TABLE search.catalog_scopes DROP CONSTRAINT catalog_scopes_scope_epoch_key;
