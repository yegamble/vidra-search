-- 0003: idempotency ledger for the event intake (POST /internal/v1/events).
--
-- Every event vidra-core delivers carries a stable event_id UUID. We record it
-- here with ON CONFLICT DO NOTHING before applying the event, so a redelivered
-- batch (at-least-once transport) is deduped: a conflicting insert marks the
-- event a duplicate and its side effects are skipped. Pruned by the retention
-- worker in a later wave.
CREATE TABLE search.events_inbox (
    event_id    UUID PRIMARY KEY,
    type        TEXT NOT NULL,
    received_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX events_inbox_received_at_idx ON search.events_inbox (received_at);
