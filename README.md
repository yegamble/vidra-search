# Vidra Search

The search, suggestion, and recommendation service for
[Vidra](https://github.com/yegamble/vidra). It is an internal microservice: only
**vidra-core** talks to it, and it returns **ranked video IDs only** — vidra-core
hydrates those IDs and applies per-viewer visibility (mutes/blocks/sensitivity).
The frontend never calls this service directly.

It serves full-text search plus prefix autosuggest, and folds behavioral events
into decayed-counter trending, personal search history, and watch affinity. A
learned LightGBM ranker is shadow-evaluated online and activated manually, and a
co-visitation index powers related/home recommendations. Per-user
search-history and full privacy-purge endpoints round out the surface.

> **Internal-only service — never publish its port to the internet.** HMAC auth
> plus network isolation are the *only* protections; the server binds `0.0.0.0`
> by default (`HTTP_HOST`). In the meta-stack only vidra-core reaches it, over
> the private compose network.

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

Storage: PostgreSQL (schema `search`, ledger table `vidra_search_migrations`) +
Redis (short-prefix suggestion cache). In the meta-stack it shares vidra-core's
Postgres and Redis using **Redis DB 1** (the standalone default is DB 0).

## Internal API

All business endpoints live under `/internal/v1` and require the HMAC header:

```
X-Vidra-Internal-Auth: v1:{unix_ts}:{hex(hmac_sha256(INTERNAL_SECRET, ts + "\n" + METHOD + "\n" + PATH))}
```

(±120s skew, constant-time compared.) The full contract is in
[`api/openapi.yaml`](api/openapi.yaml):

| Method | Path | Purpose |
|--------|------|---------|
| GET    | `/internal/v1/suggestions` | Autosuggest completions for a query prefix (doc-derived; typo fallback). Always 200. |
| GET    | `/internal/v1/search` | Ranked video IDs + scores only (hybrid full-text + trigram + filters). |
| GET    | `/internal/v1/recommendations/related` | Related videos for a seed video. |
| GET    | `/internal/v1/recommendations/home` | Home feed (trending / fresh / popular mix). |
| POST   | `/internal/v1/events` | Ingest a batch of domain + behavioral events (≤500). |
| GET    | `/internal/v1/users/{user_id}/search-history` | A user's non-hidden search history (paginated). |
| DELETE | `/internal/v1/users/{user_id}/search-history` | Clear a user's search history and anonymize their raw logs. |
| DELETE | `/internal/v1/users/{user_id}/search-history/{normalized_query}` | Delete a single search-history entry (normalized query is path-escaped). |
| DELETE | `/internal/v1/users/{user_id}` | Full privacy purge for a user (history, projections, anonymized logs). |

A drift guard (`make openapi-verify`) fails the build if the routes registered in
`internal/api` diverge from the spec. The ops probes `/healthz`, `/readyz`,
`/version`, and `/metrics` (`METRICS_ENABLED`) sit at the root and are
intentionally **not** part of the OpenAPI contract.

## Quick start

**Prerequisites:** Go 1.26 and Docker; `sqlc v1.31.1` and `migrate v4.17.1` for
codegen/migrations; Python 3.11 + [uv](https://github.com/astral-sh/uv) only for
`training/`.

```bash
# 1. Bring up the standalone stack (postgres :5433, redis :6380, api :8081)
docker compose up --build

# — or run against the deps only and iterate locally —
docker compose up -d postgres redis migrate
export DATABASE_URL=postgres://vidra_search:vidra_search@localhost:5433/vidra_search?sslmode=disable
export REDIS_URL=redis://localhost:6380/0
make run
```

The standalone compose deliberately uses non-default host ports (5433 / 6380 /
8081) so it never collides with the main stack's 5432 / 6379 / 8080.

## Configuration

**Core**

| Env | Default | Notes |
|-----|---------|-------|
| `DATABASE_URL` | dev DSN (`:5433`) | PostgreSQL DSN. Required in production. |
| `REDIS_URL` | `redis://localhost:6380/0` | Redis URL. Required in production. |
| `INTERNAL_SECRET` | dev value | HMAC secret. **Production: ≥32 bytes; the dev default is rejected.** |
| `HTTP_PORT` | `8080` | Listen port (compose maps host `8081`). |
| `HTTP_HOST` | `0.0.0.0` | Bind address. |
| `VIDRA_ENV` | `development` | `development` \| `test` \| `production`. |
| `LOG_LEVEL` | `info` | `debug` \| `info` \| `warn` \| `error`. |
| `LOG_FORMAT` | `json` | `json` \| `text`. |
| `METRICS_ENABLED` | `false` | Gates the `/metrics` scrape surface. |

**Tuning knobs**

| Env | Default | Notes |
|-----|---------|-------|
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
| `SEARCH_COVIS_INTERVAL` | `15m` | Co-visitation rollup cadence. |
| `SEARCH_COVIS_{WINDOW_SECONDS,LAMBDA,TOP_M}` | `3600/10/100` | Co-visitation window, shrinkage λ, neighbors per item. |
| `MODEL_DIR` | `/var/lib/vidra-search/models` | Directory holding learned ranker artifacts. |
| `SEARCH_MODEL_LOADER_INTERVAL` | `1m` | Active-ranker hot-swap check cadence. |
| `SEARCH_SHADOW_EVAL_INTERVAL` / `SEARCH_SHADOW_EVAL_DAYS` | `1h` / `14` | Shadow-eval cadence and impression look-back. |
| `SEARCH_RUN_JOB` | `""` | Run one named worker job once and exit (e.g. `shadow_eval`, `covis_rollup`). |

Policy knobs (`MIN_QUERY_USER_COUNT`, retention, half-life, …) are boot-time
fallbacks; vidra-core overrides them at runtime via the `search.config_updated`
event.

## Ranking models & training

`training/` holds a LightGBM **LambdaMART** pipeline (Python 3.11 + uv) that
writes versioned artifacts, computes a SHA-256, and registers each as a
`search.models` row with `status='shadow'`. The Go service shadow-evaluates
these models online against logged impressions.

Promotion is manual: `make activate-model VERSION=ranker-...` retires the active
ranker and the `model_loader` worker hot-swaps within its interval. See
[`training/README.md`](training/README.md) and
[`docs/evaluation.md`](docs/evaluation.md).

## Make targets

| Target | What it does |
|--------|--------------|
| `make ci` | Canonical gate: `fmt-check vet openapi-verify sqlc-verify test-race`. |
| `make test` / `make test-race` | Unit tests (with / without the race detector). |
| `make test-integration` | `-tags=integration` tests (self-skip without `DATABASE_URL`/`REDIS_URL`). |
| `make migrate-up` / `migrate-down` | Apply / roll back migrations (`x-migrations-table=vidra_search_migrations`). |
| `make sqlc` / `sqlc-verify` | Regenerate / drift-check typed query code (sqlc v1.31.1, pinned). |
| `make openapi-lint` / `openapi-verify` | Redocly lint / route-vs-spec drift guard. |
| `make run` / `build` | Run / build the api binary. |
| `make up` / `down` | Start / stop the standalone Docker stack. |
| `make shadow-eval` / `covis-rollup` | Run a single model shadow-eval / co-visitation rollup pass. |
| `make activate-model VERSION=…` | Promote a shadow ranker to active (retires the previous). |
| `make seed-loadtest` / `loadtest` | Seed a synthetic corpus / drive suggestions (p50/p95/p99). |
| `make check` | Local convenience gate (`fmt vet test`). |
| `make help` | List all targets. |

## CI

| Workflow | What it runs |
|----------|--------------|
| `search-ci` | `make ci` against a real Postgres 16 + Redis 7, migrations applied first. |
| `search-integration` | `-tags=integration` tests against provisioned Postgres + Redis. |
| `openapi` | Redocly lint + the route-vs-spec drift guard. |
| `training-ci` | Path-filtered to `training/`; runs the Python smoke suite. |
| `ci-guard` | Fails if `search-ci` drifts from the Makefile `ci` target. |
| `publish-container` | On release: builds a GHCR image with a provenance attestation. Never expose the resulting service to the internet. |

## Documentation

- [`docs/architecture.md`](docs/architecture.md) — component and data-flow design.
- [`docs/privacy.md`](docs/privacy.md) — retention, aggregation thresholds, and the visibility split.
- [`docs/operations.md`](docs/operations.md) — runbook, degradation, and reconciliation.
- [`docs/evaluation.md`](docs/evaluation.md) — shadow → activate runbook, labels, and metric definitions.
- [`training/README.md`](training/README.md) — the offline LightGBM ranker pipeline.

## Related repos

- [`vidra-core`](https://github.com/yegamble/vidra-core) (Go) — the source of
  truth; owns per-viewer visibility, emits events, and is the only caller of this
  service.
- [`vidra-user`](https://github.com/yegamble/vidra-user) (Next.js) — the
  frontend; talks only to vidra-core.
- [`vidra`](https://github.com/yegamble/vidra) (meta) — ties the repos together
  for local development.

## License

TBD.
