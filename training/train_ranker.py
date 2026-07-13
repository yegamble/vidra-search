#!/usr/bin/env python3
"""Offline LambdaMART ranker training for vidra-search (§1.9).

Go serves; Python trains. This script builds a labelled query→video dataset from
``query_video_engagement`` + the ``documents`` corpus, trains a LightGBM
``lambdarank`` model, writes the versioned text artifact under ``MODEL_DIR``, and
registers it in ``search.models`` with ``status='shadow'`` (never active — the Go
shadow-eval job scores it and activation is a manual step).

The FEATURE VECTOR ORDER is the contract with the Go server
(``ranking.ModelFeatureNames``); it must not be reordered without changing both
sides. The features are exactly those the server can reconstruct online from the
stage-1 recall + engagement counters, so the shadow model and the served model
see identical inputs.

Insufficient data is a HARD stop (exit code 2): a ranker trained on a handful of
labels would be worse than the always-available heuristic, so we refuse to emit a
fake model. Thresholds default to the algorithms-report guidance (~10^3–10^4
labelled query-impressions before LTR beats the linear heuristic).

Usage:
    python train_ranker.py --database-url postgres://... [--min-labeled 2000] \
        [--min-queries 200] [--version auto] [--model-dir $MODEL_DIR]

Run the synthetic smoke test (no DB, no traffic — CI):
    python train_ranker.py --smoke
"""

from __future__ import annotations

import argparse
import hashlib
import os
import sys
from dataclasses import dataclass

import numpy as np

# The feature order is the wire contract with the Go server
# (internal/ranking.ModelFeatureNames). DO NOT reorder.
FEATURES = [
    "text_rank",
    "trgm_sim",
    "exact_flags",
    "log_views",
    "age_days",
    "smoothed_ctr",
    "meaningful_rate",
    "language_match",
]

# CTR + meaningful-watch smoothing constants — MUST match the Go server
# (internal/ranking: ctrAlpha=1, ctrBeta=9, mwBeta=5).
CTR_ALPHA = 1.0
CTR_BETA = 9.0
MW_BETA = 5.0

# The SQL that assembles the per-(query, video) feature matrix + graded label.
# Text features (ts_rank_cd / trigram / exact) are computed in Postgres so they
# match the server's stage-1 recall exactly. Graded label: meaningful-watch 2,
# click 1, impression-without-click 0 (click-skip / graded relevance).
DATASET_SQL = f"""
SELECT
    qve.normalized_query AS query,
    ts_rank_cd(d.tsv, websearch_to_tsquery('simple', qve.normalized_query))::float8 AS text_rank,
    COALESCE(similarity(lower(d.title), qve.normalized_query), 0)::float8 AS trgm_sim,
    ( (CASE WHEN lower(d.title) = qve.normalized_query THEN 1.0 ELSE 0.0 END)
    + (CASE WHEN lower(coalesce(d.channel_name, '')) = qve.normalized_query THEN 0.5 ELSE 0.0 END)
    + (CASE WHEN qve.normalized_query = ANY(SELECT lower(x) FROM unnest(d.tags) AS x) THEN 0.5 ELSE 0.0 END)
    )::float8 AS exact_flags,
    ln(1 + d.views)::float8 AS log_views,
    (EXTRACT(EPOCH FROM (now() - COALESCE(d.published_at, d.source_updated_at))) / 86400.0)::float8 AS age_days,
    ((qve.clicks + {CTR_ALPHA}) / (qve.impressions + {CTR_ALPHA} + {CTR_BETA}))::float8 AS smoothed_ctr,
    (CASE WHEN qve.meaningful_watches > 0
          THEN qve.meaningful_watches::float8 / (qve.clicks + {MW_BETA})
          ELSE 0 END)::float8 AS meaningful_rate,
    0.0::float8 AS language_match,
    (CASE WHEN qve.meaningful_watches > 0 THEN 2
          WHEN qve.clicks > 0 THEN 1
          ELSE 0 END)::int AS label,
    qve.updated_at
FROM search.query_video_engagement qve
JOIN search.documents d ON d.video_id = qve.video_id
WHERE d.eligible
ORDER BY qve.updated_at, qve.normalized_query;
"""


@dataclass
class Dataset:
    X: np.ndarray  # (n, len(FEATURES))
    y: np.ndarray  # (n,) graded labels
    query: np.ndarray  # (n,) query key per row (for grouping)
    ts: np.ndarray  # (n,) updated_at ordinal (for the time split)


def build_dataset_from_db(database_url: str) -> Dataset:
    import psycopg  # imported lazily so --smoke needs no driver

    rows = []
    with psycopg.connect(database_url) as conn:
        with conn.cursor() as cur:
            cur.execute(DATASET_SQL)
            for r in cur.fetchall():
                # r = (query, *8 features, label, updated_at)
                query = r[0]
                feats = [float(x) for x in r[1:1 + len(FEATURES)]]
                label = int(r[1 + len(FEATURES)])
                ts = r[2 + len(FEATURES)]
                rows.append((query, feats, label, ts.timestamp()))
    if not rows:
        return Dataset(np.empty((0, len(FEATURES))), np.empty(0), np.empty(0, dtype=object), np.empty(0))
    X = np.array([r[1] for r in rows], dtype=np.float64)
    y = np.array([r[2] for r in rows], dtype=np.int32)
    query = np.array([r[0] for r in rows], dtype=object)
    ts = np.array([r[3] for r in rows], dtype=np.float64)
    return Dataset(X, y, query, ts)


def time_split(ds: Dataset, valid_frac: float = 0.2):
    """Split by time, keeping whole query groups on one side (rows are already
    ordered by updated_at). Returns (train_ds, valid_ds)."""
    n = len(ds.y)
    cut = int(n * (1 - valid_frac))
    # Extend the cut to a query boundary so a group is not split.
    while 0 < cut < n and ds.query[cut] == ds.query[cut - 1]:
        cut += 1
    tr = Dataset(ds.X[:cut], ds.y[:cut], ds.query[:cut], ds.ts[:cut])
    va = Dataset(ds.X[cut:], ds.y[cut:], ds.query[cut:], ds.ts[cut:])
    return tr, va


def group_sizes(query: np.ndarray) -> list[int]:
    """Contiguous group sizes for LightGBM ranking (query already sorted)."""
    sizes: list[int] = []
    if len(query) == 0:
        return sizes
    cur = query[0]
    count = 0
    for q in query:
        if q == cur:
            count += 1
        else:
            sizes.append(count)
            cur = q
            count = 1
    sizes.append(count)
    return sizes


def train_model(train: Dataset, valid: Dataset | None):
    import lightgbm as lgb

    dtrain = lgb.Dataset(train.X, label=train.y, group=group_sizes(train.query),
                         feature_name=FEATURES, free_raw_data=False)
    valid_sets = [dtrain]
    if valid is not None and len(valid.y) > 0:
        dvalid = lgb.Dataset(valid.X, label=valid.y, group=group_sizes(valid.query),
                             reference=dtrain, feature_name=FEATURES, free_raw_data=False)
        valid_sets = [dvalid]
    params = {
        "objective": "lambdarank",
        "metric": "ndcg",
        "ndcg_eval_at": [10],
        "learning_rate": 0.1,
        "num_leaves": 15,
        "min_data_in_leaf": 1,
        "max_depth": 4,
        "verbosity": -1,
    }
    booster = lgb.train(params, dtrain, num_boost_round=60, valid_sets=valid_sets)
    return booster


def make_leaves_compatible(path: str) -> None:
    """Rewrite the LightGBM model-format header so the Go `leaves` serving library
    can load it. `leaves` supports the ``version=v3`` text format; LightGBM >= 4.0
    writes ``version=v4``, which is structurally identical for the tree fields
    `leaves` reads (verified: predictions match LightGBM exactly). This ONLY
    rewrites the version tag — the trees are untouched."""
    with open(path, "r") as f:
        content = f.read()
    content = content.replace("version=v4", "version=v3", 1)
    with open(path, "w") as f:
        f.write(content)


def save_artifact(booster, model_dir: str, version: str) -> tuple[str, str]:
    os.makedirs(model_dir, exist_ok=True)
    path = os.path.join(model_dir, f"ranker-{version}.txt")
    booster.save_model(path)
    make_leaves_compatible(path)
    sha = sha256_file(path)
    return path, sha


def sha256_file(path: str) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(65536), b""):
            h.update(chunk)
    return h.hexdigest()


def register_model(database_url: str, version: str, path: str, sha: str) -> None:
    import psycopg

    with psycopg.connect(database_url) as conn:
        with conn.cursor() as cur:
            cur.execute(
                """
                INSERT INTO search.models (kind, version, status, artifact_sha256, artifact_path, metrics)
                VALUES ('ranker', %s, 'shadow', %s, %s, '{}'::jsonb)
                ON CONFLICT (kind, version) DO UPDATE
                    SET status='shadow', artifact_sha256=EXCLUDED.artifact_sha256,
                        artifact_path=EXCLUDED.artifact_path, trained_at=now()
                """,
                (version, sha, os.path.basename(path)),
            )
        conn.commit()


def synthetic_dataset(n_queries: int = 60, per_query: int = 12, seed: int = 7) -> Dataset:
    """A traffic-free synthetic dataset with a learnable signal: relevance rises
    with text_rank + smoothed_ctr. Used by the smoke test so CI can exercise the
    full training pipeline without real data."""
    rng = np.random.default_rng(seed)
    Xs, ys, qs, ts = [], [], [], []
    for qi in range(n_queries):
        for _ in range(per_query):
            text_rank = rng.uniform(0, 1)
            trgm = rng.uniform(0, 1)
            exact = rng.choice([0.0, 0.5, 1.0])
            log_views = rng.uniform(0, 12)
            age = rng.uniform(0, 365)
            ctr = rng.uniform(0, 0.5)
            mw = rng.uniform(0, 0.3)
            lang = 0.0
            # A monotone latent utility → graded label.
            utility = 2.0 * text_rank + 3.0 * ctr + 1.5 * mw + 0.2 * exact + rng.normal(0, 0.2)
            label = 2 if utility > 1.6 else (1 if utility > 0.9 else 0)
            Xs.append([text_rank, trgm, exact, log_views, age, ctr, mw, lang])
            ys.append(label)
            qs.append(f"q{qi}")
            ts.append(float(qi))
    return Dataset(np.array(Xs), np.array(ys, dtype=np.int32), np.array(qs, dtype=object), np.array(ts))


def run_smoke(out_path: str | None = None) -> int:
    """Train on synthetic data end to end and (optionally) write an artifact.
    Returns 0 on success. Exercised by pytest and the training-ci CI job."""
    ds = synthetic_dataset()
    train, valid = time_split(ds)
    booster = train_model(train, valid)
    preds = booster.predict(ds.X)
    assert preds.shape[0] == ds.X.shape[0], "prediction length mismatch"
    # Sanity: the model must have learned SOME signal (predictions vary).
    assert float(np.std(preds)) > 0, "degenerate model (no variance in predictions)"
    if out_path:
        os.makedirs(os.path.dirname(out_path) or ".", exist_ok=True)
        booster.save_model(out_path)
        make_leaves_compatible(out_path)
        print(f"smoke: wrote model to {out_path} (sha256={sha256_file(out_path)})")
    print(f"smoke: OK — trained on {len(ds.y)} rows across "
          f"{len(set(ds.query.tolist()))} queries; pred std={float(np.std(preds)):.4f}")
    return 0


def main(argv: list[str]) -> int:
    ap = argparse.ArgumentParser(description="Train the vidra-search LambdaMART ranker.")
    ap.add_argument("--database-url", default=os.environ.get("DATABASE_URL", ""))
    ap.add_argument("--model-dir", default=os.environ.get("MODEL_DIR", "./models"))
    ap.add_argument("--version", default="auto",
                    help="model version tag; 'auto' → ranker-YYYYmmddHHMMSS")
    ap.add_argument("--min-labeled", type=int, default=2000,
                    help="minimum labelled (query,video) rows required")
    ap.add_argument("--min-queries", type=int, default=200,
                    help="minimum distinct queries required")
    ap.add_argument("--no-register", action="store_true",
                    help="write the artifact but do not INSERT the models row")
    ap.add_argument("--smoke", action="store_true",
                    help="run the synthetic smoke test (no DB) and exit")
    ap.add_argument("--smoke-out", default="",
                    help="with --smoke, also write the artifact to this path")
    args = ap.parse_args(argv)

    if args.smoke:
        return run_smoke(args.smoke_out or None)

    if not args.database_url:
        print("error: --database-url (or DATABASE_URL) is required", file=sys.stderr)
        return 2

    ds = build_dataset_from_db(args.database_url)
    n_labeled = int(ds.X.shape[0])
    n_queries = len(set(ds.query.tolist()))
    n_positive = int((ds.y >= 1).sum())
    print(f"dataset: {n_labeled} rows, {n_queries} queries, {n_positive} positive labels")

    if n_labeled < args.min_labeled or n_queries < args.min_queries or n_positive == 0:
        print(
            f"error: insufficient data to train a ranker "
            f"(have {n_labeled} rows / {n_queries} queries / {n_positive} positives; "
            f"need >= {args.min_labeled} rows and >= {args.min_queries} queries with positives). "
            f"Refusing to emit a model — the heuristic ranker remains in service.",
            file=sys.stderr,
        )
        return 2

    train, valid = time_split(ds)
    booster = train_model(train, valid)

    version = args.version
    if version == "auto":
        from datetime import datetime, timezone
        version = "ranker-" + datetime.now(timezone.utc).strftime("%Y%m%d%H%M%S")

    path, sha = save_artifact(booster, args.model_dir, version)
    print(f"wrote artifact {path} (sha256={sha})")
    if not args.no_register:
        register_model(args.database_url, version, path, sha)
        print(f"registered models row: kind=ranker version={version} status=shadow")
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
