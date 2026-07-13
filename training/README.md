# vidra-search offline training

Go serves; Python trains (§1.9). This directory trains the LightGBM **LambdaMART**
ranker that the Go service can load and serve via the pure-Go
[`leaves`](https://github.com/dmitryikh/leaves) library. Training is offline and
manual; the Go `model_loader` worker picks up the active model, and the Go
`shadow_eval` job scores shadow models before any human promotes one.

## What it produces

`train_ranker.py`:

1. Assembles a labelled query→video dataset from `search.query_video_engagement`
   joined to the `search.documents` corpus. Text features (`ts_rank_cd`, trigram
   similarity, exact-match flags) are computed **in Postgres** so they match the
   server's stage-1 recall exactly.
2. Trains a LightGBM `lambdarank` model (graded labels: meaningful-watch **2**,
   click **1**, impression-without-click **0**; time-based train/valid split;
   groups = distinct normalized queries).
3. Writes `${MODEL_DIR}/ranker-<version>.txt`, computes its SHA-256, and INSERTs a
   `search.models` row with **`status='shadow'`** (never active).

The feature vector order is the **wire contract** with the Go server
(`internal/ranking.ModelFeatureNames`) — do not reorder it on one side only:

```
text_rank, trgm_sim, exact_flags, log_views, age_days, smoothed_ctr, meaningful_rate, language_match
```

The CTR/meaningful-watch smoothing constants (`α=1, β=9, mw_β=5`) also mirror the
Go server so a shadow model and the served model see identical inputs.

## Serving-format compatibility (important)

`leaves` reads the LightGBM **`version=v3`** text format. LightGBM ≥ 4.0 writes
`version=v4`, which is structurally identical for the tree fields `leaves` reads
(verified: `leaves` predictions match LightGBM's raw scores exactly). The trainer
rewrites the version header to `v3` after saving so every artifact it emits is
directly serveable — so pinning `lightgbm < 4` is **not** required.

## Install & run

With [uv](https://github.com/astral-sh/uv):

```bash
cd training
uv venv && uv pip install -e '.[dev]'

# Train against a database (writes the artifact + registers a shadow model):
uv run python train_ranker.py \
    --database-url "$DATABASE_URL" \
    --model-dir "$MODEL_DIR" \
    --min-labeled 2000 --min-queries 200
```

With plain pip:

```bash
cd training
python -m venv .venv && . .venv/bin/activate
pip install -e '.[dev]'
python train_ranker.py --database-url "$DATABASE_URL" --model-dir "$MODEL_DIR"
```

### Insufficient data is a hard stop

If the corpus has fewer than `--min-labeled` labelled rows or `--min-queries`
distinct queries (or zero positive labels), the script prints a clear message and
**exits with code 2 without emitting a model** — the always-available heuristic
ranker stays in service. A ranker trained on a handful of labels would be worse
than the heuristic (algorithms report: ~10³–10⁴ labelled query-impressions before
LTR beats the linear heuristic).

## Smoke test (no traffic, CI)

The pipeline is validated end-to-end on **synthetic** data — no database, no real
traffic — so CI can exercise dataset assembly → training → artifact save/load:

```bash
uv run python train_ranker.py --smoke                 # trains + asserts, prints OK
uv run python train_ranker.py --smoke --smoke-out /tmp/ranker.txt   # also writes an artifact
uv run pytest -q                                        # the test_smoke.py suite
```

`test_smoke.py` also asserts the feature order matches the Go contract. The CI
`training-ci` job runs `pytest -q` on every push.

## Activation & rollback

Activation is **manual** (never automatic). After shadow evaluation looks good:

```bash
make activate-model VERSION=ranker-20260713120000   # from the repo root
```

See `docs/operations.md` (model rollback) and `docs/evaluation.md`
(shadow → activate runbook, label derivation, NDCG/MRR method).

## `train_neighbors.py` — future work

Refining `item_neighbors` with iALS (Hu/Koren/Volinsky 2008) via the `implicit`
library is **future work**, deliberately not implemented here rather than faked.
The v1 neighbor index is the count-based shrunk-cosine co-visitation computed by
the Go `covis_rollup` worker, which is sufficient at PeerTube scale (algorithms
report: iALS needs ≥ 10³–10⁴ users with ≥ 5 interactions before it beats
co-visitation). When that threshold is reached, add a `train_neighbors.py` that
exports factor matrices / a materialized neighbor table and registers a
`kind='neighbors'` or `kind='factors'` model row.
