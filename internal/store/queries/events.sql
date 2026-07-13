-- name: InsertInboxEvent :one
-- Record an event id in the dedupe ledger. Returns the row only on a fresh
-- insert; a conflict (redelivery) returns no rows, which the caller reads as a
-- duplicate and skips the event's side effects.
INSERT INTO search.events_inbox (event_id, type)
VALUES (@event_id, @type)
ON CONFLICT (event_id) DO NOTHING
RETURNING event_id;
