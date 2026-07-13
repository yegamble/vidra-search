# Architecture

vidra-search is a stateless HTTP service backed by PostgreSQL and Redis. It is an
**internal** service: vidra-core is its only client, and it returns ranked video
IDs — never rendered content.

## Data flow

```
                 domain + behavioral events (idempotent, event_id UUID)
 vidra-core ───────────────────────────────────────────────▶ POST /internal/v1/events
     │                                                                │
     │                                                                ▼
     │                                                    ┌───────────────────────┐
     │  GET /internal/v1/{search,suggestions,recs}        │  search.documents     │
     ├───────────────────────────────────────────────────▶│  (projection)         │
     │                                                    │  search.events_inbox  │
     │◀────────── ranked video IDs + scores ──────────────│  search.service_config│
     │                                                    └───────────────────────┘
     ▼
 hydrate IDs (per-viewer visibility) → respond to vidra-user
```

## Packages

| Package | Responsibility |
|---------|----------------|
| `cmd/api` | Process entrypoint: config load, wiring, graceful shutdown. |
| `internal/config` | Environment configuration + validation. |
| `internal/api` | Echo server, middleware, HMAC auth, error envelope, thin handlers. |
| `internal/normalize` | The single NFKC + casefold + whitespace normalizer used everywhere. |
| `internal/event` | Idempotent event intake; applies domain events in a transaction. |
| `internal/index` | Static eligibility derivation (public + published ⇒ eligible). |
| `internal/suggest` | Suggestion pipeline (doc streams + typo fallback + blend). |
| `internal/search` | Simple-mode hybrid search. |
| `internal/recommendation` | Related and home feed composition. |
| `internal/ranking` | Pure, deterministic scoring (suggestion blend; search score constants). |
| `internal/store` | pgx pool + sqlc-generated typed queries. |
| `internal/cache` | Redis client (short-prefix suggestion cache). |
| `internal/telemetry` | slog logger + private Prometheus registry. |

## Key design decisions

- **IDs only.** Search/recs return `{video_id, score}` and never document
  content. The index bakes in only the STATIC eligibility gate; per-viewer
  visibility (mutes/blocks) is applied by vidra-core when it hydrates the IDs.
- **Idempotent intake.** Every event carries an `event_id`; the `events_inbox`
  ledger dedupes redeliveries (`ON CONFLICT DO NOTHING`). Domain events apply
  synchronously in one transaction, each inside its own savepoint so a single
  bad event is isolated in the batch response rather than poisoning the batch.
- **Normalize once.** All text matching — corpus and query — flows through
  `internal/normalize`, so folding is identical on both sides.
- **Scoring in SQL, blending in Go.** Simple search computes its score in a
  single SQL round-trip (`SearchSimple`); the suggestion blend and its weights
  live in `internal/ranking` as pure, unit-tested functions.
- **Behavioral pipeline (W2).** Behavioral events are persisted to
  `behavior_events` (plus `query_log`, and — under the `allow_history` rule —
  personal history/projection tables), then folded by cursor-based background
  workers into `query_aggregates` (global suggestions), `query_video_engagement`
  (CTR/meaningful-watch features), and Redis trending ZSETs. Ephemeral session
  context and trending increments are flushed to Redis after the DB commit. The
  aggregate-query suggestion stream is now a `query_aggregates`-backed reader.

## Storage

- Schema `search` in a PostgreSQL database that may be shared with vidra-core.
  The golang-migrate ledger lands in `vidra_search_migrations` (in `public`) so
  it never collides with core's `schema_migrations`. The runtime pool sets
  `search_path=search,public`.
- Corpus/ledger tables: `documents` (the corpus, with a generated weighted
  `tsvector` and trigram + prefix indexes), `events_inbox` (dedupe ledger), and
  `service_config` (policy overlay pushed from core).
- Behavioral tables (W2): `query_log`, `query_aggregates`, `behavior_events`,
  `user_search_history`, `user_watch_projection`, `query_video_engagement`, and
  `worker_cursors` (rollup bookmarks). Later waves add the co-visitation and
  model tables.
- Redis holds the short-prefix suggestion cache (TTL 60s, prefixes ≤3 chars),
  per-session recency lists (`sess:q` / `sess:v`, 2h TTL), the trending ZSETs +
  per-day HLL/count keys, and the gated `trend:{q,v}:top` lists.
