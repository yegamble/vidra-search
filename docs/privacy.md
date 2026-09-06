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
the flag — they carry no durable per-user projection, and they are deleted on a
history deletion or pruned by retention (below).

## Aggregation thresholds (W2)

A rare, personal query never becomes a globally-suggested phrase: a normalized
query becomes "suggestible" only once it has been issued by at least
`MIN_QUERY_USER_COUNT` (default 3) **distinct users** (exact `COUNT(DISTINCT
user_id)` over the retained window, plus, for rows with no `user_id`, the count of
distinct anonymous **subjects** — `query_log.subject_id`). Trending applies the same distinct-user floor via HyperLogLog plus a
Wilson lower-bound min-volume gate and a per-subject contribution cap, and counts
the **same** identity: the user id when signed in, else `subject_id`, else
`session_id`. So one actor spamming a query 1000× yields `distinct_users = 1` and
is neither suggestible nor trending, whether they are signed in or anonymous and
rotating `X-Vidra-Session` (both proven by
`TestIntegrationManipulationResistance`).

**Co-visitation neighbours carry the same floor, and for the same reason.**
`search.item_neighbors` is a globally-served "watch this next" index, so an edge
published off one person's browsing tells every viewer something about that
person: before this gate the only filter was `score > 0`, and one session opening
three videos published three public associations. The neighbour rebuild now
publishes a pair only once at least `MIN_QUERY_USER_COUNT` **distinct subjects**
co-visited it, counted by the identical expression — the account id when signed
in, else `subject_id`, else `session_id`. Shrinkage was never this gate:
`cooc/(cooc+λ)` ranks a one-person pair low, it still publishes it, and on a quiet
instance low is first. Support is recomputed from the retained `behavior_events`
each pass, and so is **the score**: the co-occurrence counts and the cosine's
normalization mass come out of the same pairing of the same retained rows as the
subject count. The cumulative `co_watch`/`co_search` counters that used to supply
them are retired (migration `0017` says so on the tables) — they held visits
rather than people (six co-visits by one subject was `count = 6`), and nothing
ever pruned them, so a published score outlived the events it was computed from
by an unbounded margin and a user's deletion could not reach it. Deleting the
evidence now moves the number: an event that ages out, or that a purge removes,
stops contributing on the next rollup, to the score and not only to the gate.
The honest cost is the mirror of trending's: on a small or quiet
instance, real associations now fail the gate and the related rail thins or
empties rather than ranking lower; `docs/operations.md` names the symptom and the
query that confirms it is the floor and not a broken rollup.

`subject_id` is derived in vidra-core, not here: a keyed, day-scoped pseudonym of
the connecting address, domain-separated from every other pseudonym core mints,
set only on ANONYMOUS events, stripped from any client-supplied copy, and frozen
into the outbox payload at enqueue so a replay or a second drainer replica
re-sends the identical value. The service stores the pseudonym and never sees an
address. It exists because the previous anonymous identity was `session_id`, which
arrives in a client-controlled header validated for UUID shape only — one client
rotating that header minted unlimited identities and cleared a floor of 3 from a
single request loop.

Two honest limits. Because the subject rotates daily, one determined anonymous
actor can still reach a floor of 3 by searching on 3 different UTC days; the
attack is not closed, its cost moves from three requests to three days. And
because the subject is address-derived, a NAT/CGNAT/campus egress collapses many
real people into one subject — which UNDER-counts and yields FEWER suggestions,
FEWER trending items and FEWER neighbour edges, never more. On trending and on
co-visitation that under-count is felt harder than on suggestions, because both
are ranking surfaces with a hard distinct-user gate: a query genuinely popular
behind one shared egress now fails the gate outright rather than ranking lower.
That trade is taken knowingly — the identity that used to credit those people 25×
is the same identity that credited an attacker 40×, and on the wire the two are
indistinguishable. `docs/operations.md` names the symptom, the metric and the
topology where it goes wrong. Rows carrying no subject (written before migration
0016, or anonymous requests whose address could not be derived) fall back to
`session_id`; see `docs/operations.md` for the measurement that says when that
fallback can be dropped.

The threshold is **continuously re-checked, not latched**. The rollup only
recomputes `suggestible` for queries carrying new traffic, and nothing prunes
`query_aggregates`, so a string that once cleared the floor used to stay
suggestible forever — including past the retention boundary, where the
`query_log` rows proving it cleared the floor are deleted while the suggestion
survives. The daily `suggestible_reeval` housekeeper closes that: it re-applies
the same predicate over every aggregate row against the **currently surviving**
`query_log`, so `suggestible` means "supported by evidence that still exists"
rather than "was supported once". A query whose evidence has aged out, or been
deleted by a history deletion, loses instance-wide suggestibility on the next
pass.

The threshold is automatic; the manual override is a **suggestion ban**
(`query_aggregates.banned`, written only by the `/internal/v1/suggestions/bans`
routes — see the runbook). A ban is a global property of an aggregated query
string: it stores no viewer, no attribution and no per-viewer policy, and it
removes a completion from autosuggest without hiding any video.

## Deletion (W2)

**Clearing history deletes the rows.** It does not anonymize them, and the
distinction is not a nicety — until vidra-search#37 clearing NULLed `user_id`
and kept the row, which was not a milder deletion but an inversion of one. Every
k-anonymity floor above counts distinct `user_id`s **plus**, for rows with no
`user_id`, distinct `COALESCE(subject_id, session_id)`. An attributed row carries
no `subject_id` — core mints the day-scoped pseudonym only for callers with no
account — so removing the account id dropped every one of that account's rows
onto the client-supplied session fallback, and one person who had used three
sessions stopped counting as one subject and started counting as **three**.
Measured against a real database: a pair one signed-in user co-watched in three
sessions read `subjects = 1` before the clear and `3` after, which cleared the
default floor of 3 and published the pair into the globally-served "watch this
next" index; the same move promoted a below-the-floor private query into
instance-wide autosuggest. A privacy action that RAISES a distinct-subject count
publishes exactly what the floor exists to suppress, and any single account could
trigger it deliberately. Deletion is also what makes that class of bug
impossible rather than merely fixed: removing rows can only ever lower a
distinct-subject count, never raise one.

- `DELETE /internal/v1/users/{id}/search-history` and the `user.history_deleted`
  (scope=search) event **delete** the user's `user_search_history` rows, their
  `query_log` rows, and their `behavior_events` rows — matched on the `user_id`
  column *and* on `props->>'user_id'`, because `props` stores the full original
  payload and a future event type whose handler forgot to map the column would
  otherwise leave the account id in the JSON for ever. They also drop the
  ephemeral Redis recency lists (`sess:q:`/`sess:v:`) for the sessions those rows
  named: those lists are read straight back into the account's own autosuggest,
  so leaving them would keep offering the user the queries they just cleared.
- `DELETE /internal/v1/users/{id}` and `user.history_deleted` (scope=all)
  additionally purge `user_watch_projection`. After a purge, no row anywhere
  references the user (proven by `TestIntegrationHistoryEndpointsAndPurge` and
  `TestIntegrationClearAllDeletesEveryRowThatNamesTheAccount`).
- `DELETE .../search-history/{normalized_query}` removes a single entry **and
  that query's `query_log`/`behavior_events` rows for that user**, for the same
  reason: "forget I searched X" cannot leave the account counted toward X's
  instance-wide distinct-subject floor. If they search it again it is recreated
  fresh.
- The `events_inbox` dedupe ledger is deliberately **kept**. It holds
  `(event_id, type, received_at)` and no account reference, and it is the
  tombstone that stops an at-least-once redelivery from resurrecting the deleted
  rows.

### What the derived aggregates do on their next pass

A deletion reaches the derived surfaces at their own cadence, not instantly.
Honest per surface:

| surface | how it reflects a deletion | cadence |
| --- | --- | --- |
| co-visitation `item_neighbors` | fully — the covis-v1 index is a from-scratch rebuild whose co-occurrence counts, cosine normalization mass and floor subject counts all come out of one pairing of the retained `behavior_events` | `SEARCH_COVIS_INTERVAL`, default 15 min |
| autosuggest `query_aggregates.distinct_users` / `suggestible` | fully — an exact recount over the surviving `query_log`, by the rollup for any string carrying new traffic and by `suggestible_reeval` for every row regardless | rollup `SEARCH_AGGREGATES_INTERVAL` (default 1 min); `suggestible_reeval` daily |
| `query_aggregates.total_count` / `decayed_freq` | **not at all** — cumulative counters, never recomputed. They carry no identity (a query string and a number), and the gate that decides publication is `distinct_users`, which does recompute | — |
| `query_video_engagement` | **not at all** — cumulative impression/click/watch counters folded forward by cursor and never pruned. No identity in the rows, and no k-floor reads them; the same shape vidra-search#36 removed from co-visitation, still present here | — |
| trending (Redis `hll:`/`cnt:`/`trend:`) | **not at all, by construction** — the distinct-subject count is a HyperLogLog sketch built at ingest, and an element cannot be removed from an HLL. It expires with the per-day key TTL. For the same reason trending cannot be *pushed* by a clear either: the sketch does not re-read the ledger, so this bypass never reached it | expires ≤ 8 days |
| Redis `guard:trend:{domain}:{subject}:{item}` | not deleted — the account id is in the key, but a `MATCH` sweep would walk the whole keyspace on every clear for a rate-limit token | expires with `SEARCH_TREND_CAP_WINDOW`, default 1 h |

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
