"""Traffic-free smoke test for the training pipeline (CI: training-ci job).

Exercises dataset assembly → LightGBM lambdarank training → artifact save/load on
synthetic data, so the training path is validated without any real traffic or a
database. It also asserts the feature-vector order matches the Go server contract.
"""

import os
import tempfile

import numpy as np

import train_ranker as tr


def test_feature_order_matches_go_contract():
    # Mirror of internal/ranking.ModelFeatureNames(). If the Go side changes, this
    # must change in lock-step (the vector is the training/serving wire contract).
    assert tr.FEATURES == [
        "text_rank", "trgm_sim", "exact_flags", "log_views",
        "age_days", "smoothed_ctr", "meaningful_rate", "language_match",
    ]


def test_synthetic_pipeline_trains_and_predicts():
    ds = tr.synthetic_dataset()
    assert ds.X.shape[1] == len(tr.FEATURES)
    train, valid = tr.time_split(ds)
    # The split must not straddle a query group.
    if len(valid.query) > 0:
        assert train.query[-1] != valid.query[0] or len(train.query) == 0
    booster = tr.train_model(train, valid)
    preds = booster.predict(ds.X)
    assert preds.shape[0] == ds.X.shape[0]
    assert float(np.std(preds)) > 0  # learned a non-degenerate signal


def test_group_sizes_partition_rows():
    q = np.array(["a", "a", "b", "c", "c", "c"], dtype=object)
    sizes = tr.group_sizes(q)
    assert sizes == [2, 1, 3]
    assert sum(sizes) == len(q)


def test_artifact_roundtrips():
    with tempfile.TemporaryDirectory() as d:
        out = os.path.join(d, "ranker-smoke.txt")
        assert tr.run_smoke(out) == 0
        assert os.path.exists(out)
        assert os.path.getsize(out) > 0
        # A LightGBM text model starts with a "tree" header line.
        with open(out) as f:
            head = f.read(200)
        assert "tree" in head
