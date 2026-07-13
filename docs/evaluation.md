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

The driver signs each request with the same HMAC construction vidra-core uses,
hits `/internal/v1/suggestions` with a realistic mix of 1–4 character prefixes
drawn from the seeded vocabulary, and reports **p50 / p95 / p99 / max** over all
successful responses. Because the seed corpus uses a fixed RNG seed, runs are
comparable across machines and over time.

Server-side, the `vidra_search_suggest_duration_seconds` histogram measures the
suggestion pipeline in isolation (independent of HTTP framing), and
`vidra_search_http_request_duration_seconds` captures the full request. Compare
the driver's client-side percentiles against these to separate network/framing
overhead from pipeline cost.

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
