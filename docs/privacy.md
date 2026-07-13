# Privacy

vidra-search is designed so that **policy lives in vidra-core** and this service
only executes mechanism. It never decides who may see what, and it never stores
secrets.

## The visibility split

- **Static eligibility (stored here).** A document is `eligible` only when the
  source video is public AND published (and not blocked/quarantined/owner-
  unlisted). This is safe to bake into the index because it does not depend on
  who is asking.
- **Per-viewer visibility (never stored here).** Mutes, blocks, and the viewer's
  sensitivity preference are applied by vidra-core when it hydrates the returned
  IDs. vidra-search receives only a per-request `hide_sensitive` flag and an
  `is_sensitive` marker per document — never a viewer's block/mute lists.

Because search returns IDs only and core re-filters them, a stale or
over-permissive index can never leak a video the viewer should not see.

## Personalization

The effective personalization flag is computed IN core per request (instance
setting AND user preference AND signed-in) and passed to this service as a
boolean (`personalized` / `include_history`). The service receives flags, never
policy.

## The history-collection rule (W2)

The **durable personal projections** — `user_search_history` and
`user_watch_projection` — are written ONLY from events whose payload carries
`allow_history=true` AND that are attributable to a signed-in `user_id`. Core
sets `allow_history` per instance + user policy. This is enforced in exactly one
place (`event.CollectsHistory`), is unit-tested, and is proven end-to-end by an
integration test that submits searches/plays without the flag and asserts **no**
history/projection rows are ever written.

The raw ledgers (`query_log`, `behavior_events`), the ephemeral session context
(Redis, 2h TTL), and the global trending aggregates are populated regardless of
the flag — they carry no durable per-user projection and are anonymized/pruned
(below).

## Aggregation thresholds (W2)

A rare, personal query never becomes a globally-suggested phrase: a normalized
query becomes "suggestible" only once it has been issued by at least
`MIN_QUERY_USER_COUNT` (default 3) **distinct users** (exact `COUNT(DISTINCT
user_id)` over the retained window, with a session fallback for anonymous
traffic). Trending applies the same distinct-user floor via HyperLogLog plus a
Wilson lower-bound min-volume gate and a per-user contribution cap, so one user
spamming a query 1000× yields `distinct_users = 1` and is neither suggestible nor
trending (proven by `TestIntegrationManipulationResistance`).

## Deletion & anonymization (W2)

- `DELETE /internal/v1/users/{id}/search-history` and the `user.history_deleted`
  (scope=search) event **delete** the user's `user_search_history` rows and
  **NULL** their `user_id` in `query_log` and `behavior_events` (anonymization —
  the aggregate signal survives, the attribution does not).
- `DELETE /internal/v1/users/{id}` and `user.history_deleted` (scope=all)
  additionally purge `user_watch_projection`. After a purge, no row anywhere
  references the user (proven by `TestIntegrationHistoryEndpointsAndPurge`).
- `DELETE .../search-history/{normalized_query}` removes a single entry; if the
  user searches it again it is recreated fresh.

## Retention (W2)

- `EVENT_RETENTION_DAYS` (default 90; overridable per instance via
  `search_event_retention_days`): `query_log`/`behavior_events` older than this
  are deleted by the `retention` worker (daily). autovacuum reclaims the tuples;
  no explicit VACUUM is issued.
- The `events_inbox` dedupe ledger is pruned on a 7-day horizon.
- `user_watch_projection` rows whose decayed weight has fallen below a floor are
  pruned.

## What is never stored

- No request bodies, no raw query strings in logs (only the bounded route
  template is logged).
- No secrets. `INTERNAL_SECRET` is the only secret and is never logged.
- No viewer block/mute lists, no authentication tokens.
