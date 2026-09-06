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
| `internal/search` | Simple- and advanced-mode search (two-stage funnel). |
| `internal/recommendation` | Related and home feed composition (simple + advanced). |
| `internal/ranking` | Pure, deterministic scoring: suggestion blend, simple + advanced ranker, MMR, ε-greedy, co-visitation math. |
| `internal/model` | Online model serving: leaves LightGBM loader, hot-swap, shadow evaluation. |
| `internal/experiment` | Deterministic hash-bucketed A/B assignment (RAM cache). |
| `internal/store` | pgx pool + sqlc-generated typed queries. |
| `internal/cache` | Redis client (short-prefix suggestion cache, session recency). |
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

## Advanced ranking & recommendations (W3)

Advanced mode (gated by the instance `search_mode`; simple stays the zero-data
default) adds a **two-stage funnel** and learned-model serving on top of the same
event pipeline.

- **Co-visitation.** The `covis_rollup` worker (15m) rebuilds `item_neighbors`
  from scratch every pass out of the **currently retained `behavior_events`**: it
  pairs sessionized co-watch (plays/meaningful-watches in one session) and
  co-search (results clicked for one query in a session) events once per source,
  and takes the co-occurrence counts, the normalization mass and the floor's
  subject counts from that one pairing. Neighbours are the **shrunk-cosine**
  similarity (`raw = cooc/√(totᵢ·totⱼ)`, `shrunk = raw·cooc/(cooc+λ)`, λ=10)
  blended 0.7 co-watch / 0.3 co-search, top-100 neighbors per item; serving a
  related feed is then one indexed range scan. The math lives in
  `ranking.CovisShrunkCosine` (a unit-tested mirror of the
  `RebuildCovisNeighbors` SQL).
  A pair is **published only once ≥ `MIN_QUERY_USER_COUNT` distinct subjects
  co-visited it** — autosuggest's k-anonymity floor, reused rather than
  duplicated, counting the same identity (user id, else `subject_id`, else
  `session_id`) and floored per source so a below-floor co-search neither
  publishes an edge alone nor inflates one co-watch earned
  (`ranking.CovisBlendFloored`).
  There is **no cursor and no accumulator**: the counts used to live in cumulative
  `co_watch` / `co_search` tables nothing ever pruned, so a score kept counting
  co-visits retention had deleted and a user purge could not reach them. Those
  tables are retired (migration `0017` marks them; dropped a release later) and
  the ledger is the single source of truth, which is what makes retention and
  deletion move the published score and not just the gate.
- **Advanced search.** Stage-1 SQL recall (`SearchAdvancedRecall`, ≤500) unions
  the simple hybrid recall with the query's top-clicked videos and returns rich
  per-doc + engagement columns. Stage-2 is a Go rerank (`ranking.Rerank`) over a
  hand-tuned **linear model**: text score (identical to simple), prior-centred
  smoothed CTR, meaningful-watch rate, personal/channel/session affinity, language
  match, and a creator-repetition penalty. With engagement AND personalization
  zeroed it reduces to exactly the simple ordering (unit-tested invariant), so an
  anonymous / `personalized=false` request is unchanged.
- **Advanced recommendations.** Candidates = `item_neighbors` ∪ the simple sets ∪
  session co-watch (∪ co-watch of the user's recent watches, for home). They are
  scored by base relevance + affinity + freshness − novelty, **MMR**-diversified
  (λ=0.7, Jaccard tag/category similarity), capped at 2 per channel, with a
  seed-deterministic **ε-greedy** exploration slot (ε=0.1, fresh low-view docs) and
  an accurate `reason` (co_watch / similar / trending / fresh / popular /
  subscribed-when-channel-affinity-high).
- **Model serving.** Training is offline Python (`training/`); Go only serves.
  `train_ranker.py` writes a versioned LightGBM LambdaMART text artifact + SHA-256
  and registers a `search.models` row with `status='shadow'`. The `model_loader`
  worker (1m) verifies the active ranker's checksum, loads it via the pure-Go
  `leaves` library, and hot-swaps it behind an `atomic.Pointer`; a
  missing/corrupt/malformed artifact keeps the previous model (or the always-
  available heuristic) and never touches the `models` row. Which ranker serves a
  request is chosen by **experiment** assignment (`experiment.Bucket` =
  fnv1a(salt+subject) % 100); the served `model_version` and the experiment
  variant are stamped into every response and the impression log.
- **Shadow evaluation.** The `shadow_eval` worker (1h, or `make shadow-eval`)
  replays the last N days of logged impressions + click/meaningful labels and
  scores each shadow ranker's NDCG@10 / MRR@10 against the production ordering
  actually served AND a heuristic re-rank, writing the report to `models.metrics`
  and Prometheus. **Activation is manual** (`make activate-model`) — never
  automatic.

## Storage

- Schema `search` in a PostgreSQL database that may be shared with vidra-core.
  The golang-migrate ledger lands in `vidra_search_migrations` (in `public`) so
  it never collides with core's `schema_migrations`; the migrations are embedded
  in the binary and applied by its `migrate up` subcommand — in a container,
  `docker compose run --rm api migrate up` (`internal/dbmigrate`). Both the table
  name *and* its schema are pinned in code rather than in DSN parameters, so no
  connection string can move the ledger. The runtime pool sets
  `search_path=search,public`.
- Corpus/ledger tables: `documents` (the corpus, with a generated weighted
  `tsvector` and trigram + prefix indexes), `events_inbox` (dedupe ledger), and
  `service_config` (policy overlay pushed from core).
- Behavioral tables (W2): `query_log`, `query_aggregates`, `behavior_events`,
  `user_search_history`, `user_watch_projection`, `query_video_engagement`, and
  `worker_cursors` (rollup bookmarks).
- Advanced tables (W3): `item_neighbors` (derived shrunk-cosine neighbor index,
  rebuilt from the retained event ledger each pass, one indexed range scan per
  related feed), `models`
  (the ranker registry: kind/version/status/artifact/metrics), and `experiments`
  (hash-bucketed variant definitions, cached in RAM). `co_watch` / `co_search`
  (cumulative co-occurrence counters, normalized `video_a < video_b`) still exist
  but are **retired** — nothing reads or writes them; see migration `0017`.
- Redis holds the short-prefix suggestion cache (TTL 60s, prefixes ≤3 chars),
  per-session recency lists (`sess:q` / `sess:v`, 2h TTL), the trending ZSETs +
  per-day HLL/count keys, and the gated `trend:{q,v}:top` lists.
