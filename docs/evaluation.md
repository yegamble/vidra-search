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

## Quality metrics (later waves)

Ranking quality is evaluated offline once behavioral data exists (W2+):

- **MRR@10** for autosuggest.
- **NDCG@10** for search and recommendations.
- **Recall@K** for candidate generation.
- Time-based train/test splits only (never random splits — they leak the future).

Online evaluation will use team-draft interleaving (far more sensitive than A/B
at this scale) rather than fixed-split experiments. None of these are active in
W1, which ships heuristics only; the hooks (`model_version` on every response,
one impression/click logging schema) are being put in place so the W3 learned
rankers can be shadow-evaluated against the heuristic baseline.
