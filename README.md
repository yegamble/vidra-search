# vidra-search

The search, suggestion, and recommendation service for [Vidra](https://github.com/yegamble).
It is an internal microservice: only **vidra-core** talks to it, and it returns
**ranked video IDs only** — vidra-core hydrates those IDs and applies per-viewer
visibility (mutes/blocks/sensitivity). The frontend never calls this service
directly.

> **Status: W2 (behavioral analytics + trending + privacy).** Simple-mode search,
> suggestions, and related/home recommendations work with **zero behavioral
> data**; on top of that, behavioral events are now persisted and folded by
> background workers into global query aggregates (suggestible-query gating),
> per-(query,video) engagement, decayed-counter trending (with distinct-user +
> Wilson gates), personal search history + watch affinity (under an
> `allow_history` rule), and the user history/privacy endpoints. Learned ranking
> and co-visitation recommendations arrive in W3.

## Architecture at a glance

```
vidra-user ──HTTP──▶ vidra-core ──HTTP (HMAC)──▶ vidra-search
   (frontend)          (source of truth)          (this service)
                            │  ▲                        │
                            │  └── ranked video IDs ─────┘
                            └──── domain + behavioral events ──▶ POST /internal/v1/events
```

- **vidra-core → vidra-search**: pushes the catalog as a stream of idempotent
  events (`video.upsert`, `video.suppress`, `channel.*`, `user.suppress`,
  `reconcile.*`, `search.config_updated`, plus behavioral events). vidra-search
  projects these into a denormalized, search-optimized `documents` table.
- **vidra-search → vidra-core**: answers `/internal/v1/search`,
  `/suggestions`, and `/recommendations/*` with ranked IDs + scores. It bakes in
  only the **static** eligibility gate (public + published + not suppressed) and
  an `is_sensitive` flag; per-viewer visibility stays in core.
- **Degradation**: any failure here is core's cue to fall back silently to its
  own SQL — this service never becomes a hard dependency for the frontend.

Storage: PostgreSQL (schema `search`, shares a database with core via a distinct
migrations ledger) + Redis (short-prefix suggestion cache).

## Internal API

All business endpoints live under `/internal/v1` and require the HMAC header:

```
X-Vidra-Internal-Auth: v1:{unix_ts}:{hex(hmac_sha256(INTERNAL_SECRET, ts + "\n" + METHOD + "\n" + PATH))}
```

(±120s skew, constant-time compared.) The full contract is in
[`api/openapi.yaml`](api/openapi.yaml):

| Method | Path | Purpose |
|--------|------|---------|
| GET  | `/internal/v1/suggestions` | Autosuggest completions (doc-derived; typo fallback). Always 200. |
| GET  | `/internal/v1/search` | Ranked video IDs (hybrid full-text + trigram + filters). |
| GET  | `/internal/v1/recommendations/related` | Related feed for a seed video. |
| GET  | `/internal/v1/recommendations/home` | Trending / fresh / popular home mix. |
| POST | `/internal/v1/events` | Idempotent domain + behavioral event intake (≤500/batch). |

The ops probes `/healthz`, `/readyz`, `/version`, and `/metrics`
(`METRICS_ENABLED`) sit at the root and are intentionally **not** part of the
OpenAPI contract.

## Quickstart

```bash
# 1. Bring up the standalone stack (postgres :5433, redis :6380, api :8081)
docker compose up --build

# — or run against the deps only and iterate locally —
docker compose up -d postgres redis migrate
export DATABASE_URL=postgres://vidra_search:vidra_search@localhost:5433/vidra_search?sslmode=disable
export REDIS_URL=redis://localhost:6380/0
make run
```

The standalone compose deliberately avoids the main stack's ports
(5432/6379/8080/3000).

## Configuration

| Env | Default | Notes |
|-----|---------|-------|
| `HTTP_PORT` | `8080` | Listen port (compose maps host `8081`). |
| `DATABASE_URL` | dev DSN (`:5433`) | PostgreSQL DSN. Required in production. |
| `REDIS_URL` | `redis://localhost:6380/0` | Redis URL. Required in production. |
| `INTERNAL_SECRET` | dev value | HMAC secret. **≥32 bytes required in production.** |
| `VIDRA_ENV` | `development` | `development` \| `test` \| `production`. |
| `LOG_LEVEL` | `info` | `debug` \| `info` \| `warn` \| `error`. |
| `LOG_FORMAT` | `json` | `json` \| `text`. |
| `METRICS_ENABLED` | `false` | Gates the `/metrics` scrape surface. |
| `MIN_QUERY_USER_COUNT` | `3` | Distinct-user threshold for a query to become a global suggestion / trend. |
| `EVENT_RETENTION_DAYS` | `90` | Behavioral-event (`query_log`/`behavior_events`) retention. |
| `TRENDING_HALF_LIFE_HOURS` | `6` | Trending decayed-counter half-life. |
| `MEANINGFUL_WATCH_SECONDS` | `30` | Meaningful-watch absolute threshold. |
| `MEANINGFUL_WATCH_PCT` | `30` | Meaningful-watch percent-of-duration threshold. |
| `SEARCH_QUERY_HALF_LIFE_HOURS` | `168` | Suggestion recency half-life (`query_aggregates.decayed_freq`). |
| `SEARCH_WATCH_HALF_LIFE_HOURS` | `720` | Watch-affinity decay half-life (`user_watch_projection`). |
| `SEARCH_TREND_CAP_WINDOW` | `1h` | Per-user trending contribution cap window. |
| `SEARCH_TRENDING_WILSON_FLOOR` | `0.10` | Wilson lower-bound min-volume gate for trending. |
| `SEARCH_WORKERS_ENABLED` | `true` | Run the background rollup/sweeper/retention loops. |
| `SEARCH_{AGGREGATES,ENGAGEMENT,SESSIONIZER,TRENDING,RETENTION,RECONCILE_GUARD}_INTERVAL` | `1m/5m/5m/1m/24h/10m` | Worker cadences. |
| `MODEL_DIR` | `/var/lib/vidra-search/models` | Directory holding learned ranker artifacts. |
| `SEARCH_COVIS_INTERVAL` | `15m` | Co-visitation rollup cadence. |
| `SEARCH_COVIS_{WINDOW_SECONDS,LAMBDA,TOP_M}` | `3600/10/100` | Co-visitation window, shrinkage λ, neighbors per item. |
| `SEARCH_MODEL_LOADER_INTERVAL` | `1m` | Active-ranker hot-swap check cadence. |
| `SEARCH_SHADOW_EVAL_INTERVAL` / `SEARCH_SHADOW_EVAL_DAYS` | `1h` / `14` | Shadow-eval cadence and impression look-back. |
| `SEARCH_RUN_JOB` | `""` | Run one named worker job once and exit (e.g. `shadow_eval`, `covis_rollup`). |

Policy knobs (`MIN_QUERY_USER_COUNT`, retention, half-life, …) are boot-time
fallbacks; vidra-core overrides them at runtime via the `search.config_updated`
event.

## Make targets

| Target | What it does |
|--------|--------------|
| `make ci` | Canonical gate: `fmt-check vet openapi-verify sqlc-verify test-race`. |
| `make test` / `make test-race` | Unit tests. |
| `make test-integration` | `-tags=integration` tests (self-skip without `DATABASE_URL`/`REDIS_URL`). |
| `make migrate-up` / `migrate-down` | Apply / roll back migrations (uses `x-migrations-table=vidra_search_migrations`). |
| `make sqlc` / `sqlc-verify` | Regenerate / drift-check typed query code (sqlc v1.31.1, pinned). |
| `make openapi-lint` / `openapi-verify` | Redocly lint / route-vs-spec drift guard. |
| `make run` / `build` | Run / build the api binary. |
| `make seed-loadtest` / `loadtest` | Seed a synthetic corpus / drive suggestions (p50/p95/p99). |
| `make shadow-eval` / `covis-rollup` | Run a single model shadow-eval / co-visitation rollup pass. |
| `make activate-model VERSION=…` | Promote a shadow ranker to active (retires the previous). |

Offline model training lives in [`training/`](training/README.md) (Python; Go
never trains). See [`docs/evaluation.md`](docs/evaluation.md) for the shadow →
activate runbook and label/metric definitions.

## Documentation

- [`docs/architecture.md`](docs/architecture.md) — component and data-flow design.
- [`docs/privacy.md`](docs/privacy.md) — retention, aggregation thresholds, and the visibility split.
- [`docs/operations.md`](docs/operations.md) — runbook, degradation, and reconciliation.
- [`docs/evaluation.md`](docs/evaluation.md) — performance targets and how they are measured.

## Relation to the other repos

- **vidra-core** (Go) — the source of truth; owns per-viewer visibility, emits
  events, and is the only caller of this service.
- **vidra-user** (Next.js) — the frontend; talks only to vidra-core.
- **vidra** (meta) — ties the repos together for local development.
