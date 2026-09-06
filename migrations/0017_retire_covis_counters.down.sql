-- Undo 0017: clear the retirement notices. The tables themselves were never
-- touched, so there is nothing else to restore.
COMMENT ON TABLE search.co_watch IS NULL;
COMMENT ON TABLE search.co_search IS NULL;
