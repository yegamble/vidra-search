# Evaluation

This document defines the performance targets and the exact method used to
measure them. Measured results are recorded in W7 (the whole-stack verification
wave); this file describes *how* they are produced so the numbers are
reproducible rather than asserted.

## Latency targets

| Surface | Target | Scope |
|---------|--------|-------|
| Suggestions | p95 < 50 ms | internal (this service) |
| Suggestions | p95 < 100 ms | end-to-end (through vidra-core) |
| Search | p95 < 300 ms | internal |

Reference load: 50 rps of mixed prefixes against a 100k-document corpus (and, for
suggestions, a 50k-query aggregate table once W2 lands).

## How it is measured

The harness is `scripts/loadtest` (pure Go, no external `hey`/`vegeta`
dependency), driven by the Make targets:

```bash
# 1. Seed a reproducible synthetic corpus (fixed RNG seed).
COUNT=100000 make seed-loadtest

# 2. Start the service (metrics on) against that database.
METRICS_ENABLED=true make run

# 3. Drive the suggestions endpoint and read the distribution.
RPS=50 DURATION=30s make loadtest
```

The driver signs each request with the same HMAC construction vidra-core uses and
reports **p50 / p95 / p99 / max** over all successful responses. The workload is
endpoint-appropriate (`-endpoint`): `suggestions` fires a realistic mix of 1–4
character **prefixes** (the autocomplete workload); `search` fires specific
full-word **topic queries** (each resolving to a handful of documents, like a real
user query — short 1–2 char prefixes are not a representative search workload, and
pg_trgm cannot use its index below 3 characters). Because the seed corpus uses a
fixed RNG seed, runs are comparable across machines and over time.

The seeded corpus weaves a large synthetic topic vocabulary (`numTopics`) into
every title so that query selectivity mirrors a real catalogue: with only the
small adjective/noun vocabulary every term would match 5–14 % of the corpus
(pathologically dense), so a topic word is used per title and a topic query
matches ≈ `n / numTopics` documents (index-driven recall, not a full scan).

Server-side, the `vidra_search_suggest_duration_seconds` histogram measures the
suggestion pipeline in isolation (independent of HTTP framing), and
`vidra_search_http_request_duration_seconds{route}` captures the full request per
endpoint. Compare the driver's client-side percentiles against these to separate
network/framing overhead from pipeline cost.

## Results (W7 — 2026-07-13)

Environment: local Docker on Apple Silicon (darwin/arm64), Postgres 16 + Redis 7
(vidra-search standalone stack, host ports 5433/6380/8082), Go 1.26.2. Corpus:
100 000 synthetic documents (fixed RNG seed). Load: 50 rps for 30 s per run after
a 10 s warm-up. Client percentiles are from `scripts/loadtest`; server percentiles
are `histogram_quantile` over the service's own histograms.

| Surface | Target | Client p50 / p95 / p99 | Server p95 | Result |
|---------|--------|------------------------|-----------|--------|
| Suggestions — internal | p95 < 50 ms | 0.8 / 37.6 / 48.7 ms | ≤ 50 ms (`suggest_duration`) | **PASS** |
| Search — internal | p95 < 300 ms | 4.7 / 6.9 / 13.0 ms | ≤ 10 ms (`http…{route="/internal/v1/search"}`) | **PASS** |
| Suggestions — end-to-end (through vidra-core) | p95 < 100 ms | 6.6 / 10.0 / 13.2 ms | — | **PASS** |

All runs completed with **zero errors** (`ok=1499 / 1499` internal; e2e against the
live 11-document stack).

Caveats & fixes made during W7:

- **Index-driven recall (migration `0014` + query rewrite).** The simple/advanced
  search recall and the suggestion typo-fallback originally used the *function*
  form `similarity(lower(title), q) >= 0.3`, which cannot use a trigram index and
  forced a **sequential scan of every document on every query** (~450 ms single
  request at 100k, collapsing to multi-second timeouts under 50 rps). The recall
  now uses the `%` operator against a new `lower(title) gin_trgm_ops` index (plus
  `tags @> ARRAY[q]` and an index-usable channel-name equality), giving a
  BitmapOr across the tsv/trigram/tags/channel indexes (~0.4 ms for a selective
  query). This is the change that makes the search target reachable at scale.
- **Representative corpus.** The seeder now weaves the topic vocabulary described
  above; without it every query matched a large fraction of the tiny vocabulary
  and no query was ever selective (unlike a real catalogue).
- The e2e figure is measured against the running whole-stack (11 documents), so it
  reflects core routing + the HMAC S2S hop rather than large-corpus query cost;
  the internal search figure above is the 100k-corpus number.

## Quality metrics

Ranking quality is evaluated offline:

- **MRR@10** for autosuggest.
- **NDCG@10** for search and recommendations.
- **Recall@K** for candidate generation.
- Time-based train/test splits only (never random splits — they leak the future).

Online evaluation will use team-draft interleaving (far more sensitive than A/B
at this scale) rather than fixed-split experiments.

## Label derivation (W3)

Both training and shadow evaluation derive **graded relevance** labels from the
one logging schema, per `(query, video)` impression:

| Signal | Graded label |
|--------|--------------|
| meaningful-watch (≥30s or ≥30% of duration) | **2** |
| click (`search.result_clicked`) without a meaningful watch | **1** |
| impression (`video.impression`) with no click | **0** |

This is the standard click-skip / graded-relevance construction (Joachims). An
impression list for shadow evaluation is a `(session, normalized_query)` group of
`video.impression` rows, each labelled by whether that video was later clicked /
meaningful-watched in the same session.

## NDCG@10 / MRR@10 method

Both metrics take the graded labels **in the order a ranker placed them**, so the
same helpers (`model.NDCGAt`, `model.MRRAt`) score three orderings per impression
list:

- **production** — the order the videos were actually served (impression
  position);
- **shadow** — the shadow model's re-rank of the same videos over the recall
  features;
- **heuristic** — the heuristic ranker's re-rank.

`NDCG@10` uses the standard `2^rel − 1` gain with a `log₂(i+2)` discount,
normalized by the ideal ordering of the same labels; `MRR@10` is the reciprocal
rank of the first relevant (rel ≥ 1) item. Groups with fewer than 2 items or no
positive label are skipped (no ranking signal). The shadow report written to
`models.metrics` records the mean over groups plus the deltas `vs_production` and
`vs_heuristic`.

## Data thresholds

A learned ranker only beats the linear heuristic with enough labelled data
(algorithms report: ~10³–10⁴ labelled query-impressions). `train_ranker.py`
enforces this with `--min-labeled` (default 2000 rows) and `--min-queries`
(default 200 distinct queries); below either — or with zero positives — it
**exits 2 without emitting a model**, leaving the heuristic in service. Neighbor
refinement (iALS) has a similar threshold (≥10³–10⁴ users with ≥5 interactions)
and is deferred until then (`training/README.md`).

## Shadow → activate runbook

1. **Train** a shadow model (writes the artifact + a `status='shadow'` row):
   ```bash
   cd training && uv run python train_ranker.py \
       --database-url "$DATABASE_URL" --model-dir "$MODEL_DIR"
   ```
2. **Shadow-evaluate** it against production + heuristic orderings:
   ```bash
   make shadow-eval            # one pass; also runs hourly as the shadow_eval worker
   ```
   Read the result from the model row (or the `vidra_search_shadow_eval` gauges):
   ```sql
   SELECT version, metrics FROM search.models WHERE kind='ranker' AND status='shadow';
   ```
   Promote only when `ndcg@10` and `vs_production` / `vs_heuristic` are convincingly
   positive over a meaningful number of `groups`.
3. **Activate** (manual — retires the previous active ranker; the `model_loader`
   worker hot-swaps within its interval):
   ```bash
   make activate-model VERSION=ranker-20260713120000
   ```
4. **Roll back** by re-activating the previous version (see `docs/operations.md`).

The impression log stamps `model_version` on every served result, so post-hoc
analysis can attribute outcomes to the ranker (and experiment variant) that
produced them.
