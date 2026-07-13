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

Route labels are always the bounded Echo template — never a raw URL — so
cardinality stays flat.

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
  drift stale. A `reconcile_guard` worker that alarms on this lands in a later
  wave; until then, watch the `vidra_search_documents` gauge against the expected
  catalog size.

## Common tasks

- **Apply migrations**: `make migrate-up` (uses
  `x-migrations-table=vidra_search_migrations`).
- **Regenerate typed queries** after a SQL change: `make sqlc` then commit
  `internal/store/sqlcgen`; `make sqlc-verify` guards drift in CI.
- **Reseed a load-test corpus**: `COUNT=100000 make seed-loadtest`.
