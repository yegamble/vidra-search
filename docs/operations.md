# Operations runbook

## Health & readiness

- `GET /healthz` — process liveness; always 200 while the process is up.
- `GET /readyz` — dependency readiness; 200 when Postgres and Redis both ping,
  else 503 with per-component detail (`{"status":"degraded","components":{...}}`).
  Wire orchestrator readiness gates to this.
- `GET /version` — build metadata (version/commit/date/go).
- `GET /metrics` — Prometheus scrape (only when `METRICS_ENABLED=true`).

## Metrics

All metrics use the `vidra_search_` prefix on a private registry:

- `vidra_search_http_requests_total{method,route,status_class}` — RED rate/errors.
- `vidra_search_http_request_duration_seconds{method,route,status_class}` — latency.
- `vidra_search_suggest_duration_seconds` — the suggestion pipeline in isolation.
- `vidra_search_events_total{type,outcome}` — event intake, `outcome ∈
  {accepted,duplicate,failed,ignored,counted}`. A rising `failed` rate means core
  is sending malformed payloads.
- `vidra_search_documents{eligible}` — corpus size by static eligibility
  (scrape-time gauge). A collapse to zero indicates an index wipe or a broken
  event feed.
- `vidra_search_event_lag_seconds` — delay between an event's `occurred_at` and
  its processing at intake. A rising tail means core's outbox is backing up.
- `vidra_search_rollup_duration_seconds{worker}` — background worker pass time.
- `vidra_search_worker_errors_total{worker}` — worker pass failures.
- `vidra_search_trending_gate_rejections_total{domain,reason}` — trending
  candidates rejected, `reason ∈ {distinct_users, wilson_min_volume}`. A high
  ratio is expected and healthy (the gates are doing their job).
- `vidra_search_reconcile_age_seconds` — seconds since the last `reconcile.end`
  (`-1` if none on record). Alert if it exceeds ~48h.
- `vidra_search_table_rows{table}` — approximate row counts (planner statistics)
  for the search schema, sampled at scrape time.

Route labels are always the bounded Echo template — never a raw URL — so
cardinality stays flat.

### Key PromQL queries

Copy-paste queries for the critical signals (dashboards + alert rules). All
`_bucket` histograms need the `sum by (le)` regrouping for `histogram_quantile`.

- **Suggestion latency (internal pipeline) p95/p99** — target p95 < 50 ms:
  ```promql
  histogram_quantile(0.95, sum by (le) (rate(vidra_search_suggest_duration_seconds_bucket[5m])))
  histogram_quantile(0.99, sum by (le) (rate(vidra_search_suggest_duration_seconds_bucket[5m])))
  ```
- **Search latency (internal) p95** — target p95 < 300 ms:
  ```promql
  histogram_quantile(0.95, sum by (le) (
    rate(vidra_search_http_request_duration_seconds_bucket{route="/internal/v1/search"}[5m])))
  ```
  (swap the route for `/internal/v1/suggestions` to see suggestion latency incl.
  HTTP framing; and `sum by (le, route)` to chart every endpoint at once.)
- **Event lag** (outbox → index freshness of *behaviour*) p95, seconds:
  ```promql
  histogram_quantile(0.95, sum by (le) (rate(vidra_search_event_lag_seconds_bucket[5m])))
  ```
  A rising tail means core's outbox is backing up — cross-check the outbox drain
  on the core side.
- **Queue / rollup backlog depth** — the worker-input tables (rows awaiting a
  rollup pass) and worker health:
  ```promql
  vidra_search_table_rows{table=~"behavior_events|query_log|events_inbox"}
  histogram_quantile(0.95, sum by (le, worker) (rate(vidra_search_rollup_duration_seconds_bucket[15m])))
  sum by (worker) (rate(vidra_search_worker_errors_total[15m]))
  ```
  Backlog that grows monotonically while `rollup_duration` is flat means a worker
  is wedged (check `worker_errors_total`).
- **Index freshness** — staleness of the document index vs the source catalogue:
  ```promql
  vidra_search_reconcile_age_seconds                 # seconds since last reconcile.end; alert > 48h (172800)
  vidra_search_documents{eligible="true"}            # eligible corpus size; alert on a sharp drop
  ```
- **Unsafe-impression proxy.** Per-viewer visibility (mutes/blocks/sensitivity) is
  enforced in vidra-core when it hydrates the returned ids, so the true
  unsafe-impression rate is a core metric; the search-side proxies are the safety
  gates firing and the share of ineligible docs the index is holding (which the
  queries filter out but must never serve):
  ```promql
  sum by (reason) (rate(vidra_search_trending_gate_rejections_total[5m]))   # safety gates actively rejecting
  vidra_search_documents{eligible="false"}
    / ignoring(eligible) sum without(eligible) (vidra_search_documents)     # ineligible share of the index
  ```
  A trending-gate rejection rate that drops to zero (gates stopped firing) or an
  ineligible share trending toward the eligible set are the signals to alert on.

## Background workers (W2)

The single binary runs six ticker loops (cadences env-tunable; disable all with
`SEARCH_WORKERS_ENABLED=false`). The cursor-based rollups advance their bookmark
in `worker_cursors` in the SAME transaction as the writes they cover, so a crash
resumes rather than reprocesses; derived rows use deterministic ids so retries
are idempotent.

- `aggregates_rollup` (1m) — folds new `query_log` into `query_aggregates`
  (decay-then-increment `decayed_freq`, exact distinct-user recount, suggestible
  flag).
- `engagement_rollup` (5m) — derives `video.meaningful_watch` from qualifying
  `video.watch_progress`, folds impressions/clicks/meaningful-watches into
  `query_video_engagement`, and applies the meaningful-watch projection weight.
- `sessionizer` (5m) — derives `search.reformulated` / `search.abandoned` over
  settled `query_log` rows.
- `trending_sweeper` (1m) — decays + prunes the Redis trend ZSETs and republishes
  the gated `trend:q:top` / `trend:v:top` lists.
- `retention` (24h) — deletes aged events, prunes the inbox + low-weight
  projections.
- `reconcile_guard` (10m) — warns + sets the reconcile-age gauge when no
  `reconcile.end` has arrived within `~48h`.

W3 adds four more loops (wired as generic periodic jobs; also runnable one-shot
via `SEARCH_RUN_JOB=<name>`):

- `covis_rollup` (15m, cursor-based) — folds sessionized co-watch/co-search pairs
  into the cumulative counters and rebuilds the `item_neighbors` shrunk-cosine
  index (λ=`SEARCH_COVIS_LAMBDA`=10, top-M=`SEARCH_COVIS_TOP_M`=100, window
  `SEARCH_COVIS_WINDOW_SECONDS`=3600).
- `model_loader` (1m) — hot-swaps the active learned ranker (checksum-verified)
  behind an atomic pointer; a bad artifact keeps the previous model/heuristic.
- `shadow_eval` (1h) — scores shadow rankers over recent impressions
  (`SEARCH_SHADOW_EVAL_DAYS`=14 look-back); writes `models.metrics` + gauges.
- `experiment_refresh` (5m) — reloads enabled experiment definitions into RAM.

W3 metrics:

- `vidra_search_loaded_model{kind,version}` — the currently-served model (value 1
  on the active version label); `kind=ranker version=heuristic-v1` means no learned
  model is loaded.
- `vidra_search_model_load_errors_total` — learned-artifact load failures. Any
  increase means an active model is missing/corrupt/malformed and the service is
  falling back — investigate the artifact + its `models` row.
- `vidra_search_shadow_eval{version,metric}` — shadow NDCG@10 / MRR@10 and the
  `vs_production` / `vs_heuristic` deltas per shadow model version.

Trending gates (before exposure): a distinct-user floor (HLL) `≥
MIN_QUERY_USER_COUNT`, a Wilson lower-bound min-volume gate
(`SEARCH_TRENDING_WILSON_FLOOR`, default 0.10), and a per-user contribution cap
(`SEARCH_TREND_CAP_WINDOW`, default 1h).

## Degradation contract

vidra-search is a **soft dependency**. Every failure mode is core's cue to fall
back silently, so the frontend never sees a 5xx caused by search:

- **suggestions** → the pipeline itself already returns `200` with an empty list
  on any internal error; core additionally falls back to a local title-prefix
  query.
- **search** → core falls back to its own `SearchPublicVideos` SQL.
- **recommendations** → core falls back to trending/recent (home) or
  same-channel/same-category (related).

Operationally: if this service is unhealthy, discovery quality degrades but the
platform keeps serving. Do not page on a single search outage unless it is
sustained; do alert on `readyz` flapping and on a rising event `failed` rate.

## Event intake

- `POST /internal/v1/events` accepts ≤500 events per batch. Each is deduped via
  `events_inbox` and applied inside its own savepoint, so one bad event lands in
  the `failed[]` array without failing the batch.
- Replays are safe by construction: an identical batch returns all `duplicates`
  with zero state change.

## Reconciliation

vidra-core periodically re-sends the full eligible catalog as
`reconcile.begin` → `reconcile.page`* → `reconcile.end`, stamping every touched
document with the run's `reconcile_run_id`. On `reconcile.end`, any eligible
LOCAL document NOT stamped by the current run is suppressed with reason
`reconcile_orphan` — this is how deletions/unpublishes that missed their
individual event are eventually reconciled.

- If reconciliation stops (no `reconcile.end` for a long window), the index can
  drift stale. The `reconcile_guard` worker (W2) sets
  `vidra_search_reconcile_age_seconds` and logs a warning past ~48h; also watch
  the `vidra_search_documents` gauge against the expected catalog size.

## Model registry & rollback (W3)

Learned rankers are trained offline (`training/`), registered as
`status='shadow'`, shadow-evaluated, then **manually** activated. See
`docs/evaluation.md` for the full shadow → activate runbook. Serving never depends
on a learned model: the heuristic ranker is always available, and any
load/checksum failure falls back to it (`vidra_search_model_load_errors_total`).

- **Activate a shadow model** (retires the previous active ranker; the
  `model_loader` worker hot-swaps within ~1m):
  ```bash
  make activate-model VERSION=ranker-20260713120000
  ```
- **Roll back to the previous version** — re-activate it (this retires the current
  one). If you know the previous version:
  ```bash
  make activate-model VERSION=ranker-20260701090000
  ```
  Or by hand:
  ```sql
  BEGIN;
  UPDATE search.models SET status='retired' WHERE kind='ranker' AND status='active';
  UPDATE search.models SET status='active', activated_at=now()
    WHERE kind='ranker' AND version='<previous-version>';
  COMMIT;
  ```
- **Emergency: disable the learned ranker entirely** — retire the active row; with
  no active ranker the loader reverts to the heuristic on its next pass:
  ```sql
  UPDATE search.models SET status='retired' WHERE kind='ranker' AND status='active';
  ```
- **Remove a bad artifact** — after retiring its row, delete
  `${MODEL_DIR}/ranker-<version>.txt`. A corrupt/missing artifact for an active
  model is already non-fatal (the loader keeps the previous model + logs +
  increments the load-error metric, and does not modify the `models` row).

Confirm the served model with `vidra_search_loaded_model{kind="ranker"}` or:
```sql
SELECT version, status, activated_at FROM search.models WHERE kind='ranker' ORDER BY id DESC;
```

## Common tasks

- **Apply migrations**: `make migrate-up` (uses
  `x-migrations-table=vidra_search_migrations`).
- **Regenerate typed queries** after a SQL change: `make sqlc` then commit
  `internal/store/sqlcgen`; `make sqlc-verify` guards drift in CI.
- **Reseed a load-test corpus**: `COUNT=100000 make seed-loadtest`.
- **Run a rollup once** (debug): `SEARCH_RUN_JOB=covis_rollup make covis-rollup`,
  or `make shadow-eval`.
- **Train a shadow ranker**: see `training/README.md` and `docs/evaluation.md`.
