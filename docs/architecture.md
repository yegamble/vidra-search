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

- **Co-visitation.** The `covis_rollup` worker (15m, cursor-based) folds
  sessionized co-occurrence into cumulative `co_watch` (plays/meaningful-watches
  in one session) and `co_search` (results clicked for one query in a session)
  counters, then rebuilds `item_neighbors` as the **shrunk-cosine** similarity
  (`raw = cooc/√(totᵢ·totⱼ)`, `shrunk = raw·cooc/(cooc+λ)`, λ=10) blended
  0.7 co_watch / 0.3 co_search, top-100 neighbors per item. Serving a related feed
  is then one indexed range scan. The math lives in `ranking.CovisShrunkCosine`
  (a unit-tested mirror of the `RebuildCovisNeighbors` SQL).
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
  it never collides with core's `schema_migrations`. The runtime pool sets
  `search_path=search,public`.
- Corpus/ledger tables: `documents` (the corpus, with a generated weighted
  `tsvector` and trigram + prefix indexes), `events_inbox` (dedupe ledger), and
  `service_config` (policy overlay pushed from core).
- Behavioral tables (W2): `query_log`, `query_aggregates`, `behavior_events`,
  `user_search_history`, `user_watch_projection`, `query_video_engagement`, and
  `worker_cursors` (rollup bookmarks).
- Advanced tables (W3): `co_watch` / `co_search` (cumulative co-occurrence
  counters, normalized `video_a < video_b`), `item_neighbors` (derived
  shrunk-cosine neighbor index, one indexed range scan per related feed), `models`
  (the ranker registry: kind/version/status/artifact/metrics), and `experiments`
  (hash-bucketed variant definitions, cached in RAM).
- Redis holds the short-prefix suggestion cache (TTL 60s, prefixes ≤3 chars),
  per-session recency lists (`sess:q` / `sess:v`, 2h TTL), the trending ZSETs +
  per-day HLL/count keys, and the gated `trend:{q,v}:top` lists.
