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

W1 is fully non-personalized. When personalization arrives (W2+), the effective
personalization flag is computed IN core per request (instance setting AND user
preference AND signed-in) and passed to this service as a boolean. The service
receives flags, never policy.

## Aggregation thresholds (W2)

To ensure a rare, personal query never becomes a globally-suggested phrase, a
normalized query only becomes "suggestible" once it has been issued by at least
`MIN_QUERY_USER_COUNT` (default 3) **distinct users**. Distinct-user counting
uses HyperLogLog so raw identities are not retained for the guard. These
aggregate tables and the guard land in W2; W1 ships the config knob and the
no-op seam only.

## Retention (W2)

- `EVENT_RETENTION_DAYS` (default 90): behavioral events older than this are
  deleted by the retention worker (W2).
- The `events_inbox` dedupe ledger is pruned on a short horizon (it only needs
  to outlive redelivery windows).
- `user.history_deleted` and user-deletion events purge the affected user's
  history and watch projections. In W1 these projections do not exist yet, so
  the events are accepted as no-ops; the handlers become destructive in W2.

## What is never stored

- No request bodies, no raw query strings in logs (only the bounded route
  template is logged).
- No secrets. `INTERNAL_SECRET` is the only secret and is never logged.
- No viewer block/mute lists, no authentication tokens.
