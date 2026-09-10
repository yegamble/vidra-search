#!/usr/bin/env bash
# Fan-in gate (A39 / QLT-01, SC3): collapse this repo's several required
# workflows into ONE named check an owner can put in branch protection.
#
# GitHub cannot express `needs:` across workflows, and this repo's required
# lanes live in separate files on purpose (different services, different
# runtimes, different path filters). So the fan-in reads the checks API for the
# commit under test and enforces a CHECKED-IN manifest of what "required" means:
#
#   .github/required-checks.txt
#     name          must EXIST and must conclude `success`
#     ?name         required only IF it ran (path-filtered lanes such as
#                   schema-compat, which only trigger on migrations/ changes)
#
# Failure modes it closes:
#   * a required lane silently stops triggering (renamed job, broken `on:`,
#     an over-eager `paths:` filter) — the manifest still demands it, so the
#     fan-in fails instead of the PR merging with one fewer proof;
#   * a lane that concludes `cancelled`, `timed_out`, `neutral` or `skipped`
#     being read as "not failed" — only `success` passes here;
#   * branch protection drifting behind the workflow set — the manifest is the
#     single place both are declared;
#   * a workflow FILE GitHub rejects — a run with zero jobs produces no
#     check-run, so a `?name` lane would otherwise vanish without a trace.
#
# It deliberately does NOT create or modify branch protection. The owner
# configures exactly one required check, `ci-required`, per repo.
#
# Env: GH_TOKEN (checks:read), GITHUB_REPOSITORY, and the SHA to inspect.
set -euo pipefail

manifest=${MANIFEST:-.github/required-checks.txt}
repo=${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}
sha=${CHECK_SHA:?CHECK_SHA is required}
deadline_min=${DEADLINE_MINUTES:-75}
interval=${POLL_SECONDS:-20}

[ -r "$manifest" ] || { echo "::error::ci-required: manifest $manifest is missing" >&2; exit 1; }

required=$(grep -vE '^[[:space:]]*(#|$)' "$manifest" | sed 's/[[:space:]]*$//')
[ -n "$required" ] || { echo "::error::ci-required: $manifest lists no checks" >&2; exit 1; }

echo "ci-required: commit ${sha}"
echo "ci-required: manifest ${manifest}"
printf '%s\n' "$required" | sed 's/^/  - /'

deadline=$(( $(date +%s) + deadline_min * 60 ))

while :; do
  # `--paginate` because a busy commit can carry more than one page of runs, and
  # a missed page would look exactly like a lane that never started.
  runs=$(gh api --paginate -H "Accept: application/vnd.github+json" \
    "repos/${repo}/commits/${sha}/check-runs?per_page=100" \
    --jq '.check_runs[] | [.name, .status, (.conclusion // "")] | @tsv' 2>/dev/null || true)

  # --- a run GitHub could not even start ---------------------------------------
  # A workflow whose FILE GitHub rejects (a duplicated key, an action ref that
  # does not resolve) still produces a run: conclusion "failure", zero jobs —
  # and, the part that matters here, no check-run at all. The loop above reads
  # check-runs, so it cannot see it, and a `?name` lane simply vanishes from
  # the manifest's point of view. vidra-search's rollback-floor.yml failed
  # exactly like this on every push from 2026-09-08 to 2026-09-10 with this
  # gate green. Zero jobs plus "failure" has no other meaning, so it fails here
  # by name, whether or not the manifest lists the lane.
  empty_failures=$(gh api --paginate -H "Accept: application/vnd.github+json" \
    "repos/${repo}/actions/runs?head_sha=${sha}&per_page=100" \
    --jq '.workflow_runs[] | select(.conclusion == "failure") | [.id, .path] | @tsv' 2>/dev/null || true)
  while IFS=$'\t' read -r run_id wf_path; do
    [ -n "$run_id" ] || continue
    jobs=$(gh api "repos/${repo}/actions/runs/${run_id}/jobs?per_page=1" --jq '.total_count' 2>/dev/null || echo "?")
    if [ "$jobs" = "0" ]; then
      echo "::error::ci-required: GitHub rejected the workflow file ${wf_path} (run ${run_id} failed with zero jobs — a duplicated key, or an action that does not resolve). Nothing that file defines can run for ${sha}." >&2
      exit 1
    fi
  done <<EOM
$empty_failures
EOM

  pending=""
  missing=""
  failed=""

  while IFS= read -r entry; do
    [ -n "$entry" ] || continue
    optional=0
    name=$entry
    if [ "${name#\?}" != "$name" ]; then optional=1; name=${name#\?}; fi

    line=$(printf '%s\n' "$runs" | awk -F'\t' -v n="$name" '$1 == n { print; exit }')
    if [ -z "$line" ]; then
      if [ "$optional" -eq 1 ]; then
        echo "  (not triggered, optional-if-absent): ${name}"
      else
        missing="${missing}${name}"$'\n'
      fi
      continue
    fi
    status=$(printf '%s' "$line" | cut -f2)
    concl=$(printf '%s' "$line" | cut -f3)
    if [ "$status" != "completed" ]; then
      pending="${pending}${name} (${status})"$'\n'
    elif [ "$concl" != "success" ]; then
      failed="${failed}${name} -> ${concl}"$'\n'
    fi
  done <<EOM
$required
EOM

  if [ -n "$failed" ]; then
    echo "::error::ci-required: a required lane did not succeed:" >&2
    printf '%s' "$failed" | sed 's/^/  /' >&2
    exit 1
  fi

  if [ -z "$pending" ] && [ -z "$missing" ]; then
    echo "OK: every required check on ${sha} concluded success."
    exit 0
  fi

  now=$(date +%s)
  if [ "$now" -ge "$deadline" ]; then
    echo "::error::ci-required: gave up after ${deadline_min} minutes." >&2
    [ -n "$pending" ] && { echo "  still running:" >&2; printf '%s' "$pending" | sed 's/^/    /' >&2; }
    [ -n "$missing" ] && { echo "  never started (a required lane is not triggering for this commit):" >&2; printf '%s' "$missing" | sed 's/^/    /' >&2; }
    exit 1
  fi

  [ -n "$pending" ] && printf 'waiting: %s' "$(printf '%s' "$pending" | tr '\n' ' ')"
  [ -n "$missing" ] && printf 'not yet started: %s' "$(printf '%s' "$missing" | tr '\n' ' ')"
  echo
  sleep "$interval"
done
