-- lower(title) trigram GIN index for the hybrid search / suggestion recall.
--
-- The simple + advanced search recall and the suggestion typo-fallback match
-- titles case-insensitively. The original predicate used the FUNCTION form
-- `similarity(lower(title), q) >= 0.3`, which cannot use a trigram index and
-- therefore forced a full sequential scan over every document on every query
-- (the dominant cost at scale — a 100k-doc corpus scanned per request). The
-- queries now use the `%` operator (`lower(title) % q`), which is index-driven
-- via this functional GIN index (recall threshold = pg_trgm.similarity_threshold,
-- default 0.3, matching the previous constant). pg_trgm is enabled in 0001; the
-- existing documents_title_trgm_idx is on `title` (not lower(title)), so it does
-- not serve the case-folded predicate.
CREATE INDEX documents_lower_title_trgm_idx
    ON search.documents USING GIN (lower(title) gin_trgm_ops);
