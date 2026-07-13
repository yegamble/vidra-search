-- 0010: durable bookmarks for the incremental rollup workers.
--
-- Each cursor-based worker (aggregates_rollup, engagement_rollup, sessionizer)
-- records the highest source-table id it has folded, so a restart resumes rather
-- than reprocesses. The cursor is advanced in the SAME transaction as the writes
-- it covers, so a crash mid-batch rolls back both and the batch is retried; the
-- deterministic-id ON CONFLICT dedupe on derived rows makes the retry idempotent.
CREATE TABLE search.worker_cursors (
    cursor_name TEXT PRIMARY KEY,
    cursor_pos  BIGINT NOT NULL DEFAULT 0,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
