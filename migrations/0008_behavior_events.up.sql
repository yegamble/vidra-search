-- 0008: the behavioral event ledger.
--
-- One durable row per behavioral event (submitted/clicked/played/…), plus the
-- synthetic events the workers derive (video.meaningful_watch, search.reformulated,
-- search.abandoned). event_id is UNIQUE so both at-least-once redelivery and
-- idempotent re-derivation (deterministic uuid_generate_v5 keys) collapse to a
-- single row. props keeps the full original payload so the rollup/sessionizer
-- workers can read fields (position_seconds, duration_seconds, allow_history, …)
-- that do not warrant a dedicated column.
CREATE TABLE search.behavior_events (
    id               BIGSERIAL PRIMARY KEY,
    event_id         UUID NOT NULL UNIQUE,
    type             TEXT NOT NULL,
    user_id          UUID,
    session_id       TEXT,
    normalized_query TEXT,
    video_id         UUID,
    position         INT,
    model_version    TEXT,
    occurred_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    props            JSONB NOT NULL DEFAULT '{}'
);

-- Rollups/sessionizer scan by id cursor; retention deletes by occurred_at.
CREATE INDEX behavior_events_occurred_at_idx ON search.behavior_events (occurred_at);
-- Type-scoped scans (engagement fold, meaningful-watch derivation).
CREATE INDEX behavior_events_type_occurred_idx ON search.behavior_events (type, occurred_at);
-- Sessionizer / meaningful-watch attribution look up by (session, video).
CREATE INDEX behavior_events_session_video_idx ON search.behavior_events (session_id, video_id);
