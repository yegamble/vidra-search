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
#     check-run, so a `?name` lane would otherwise vanish without a trace;
#   * an API read that FAILED being taken for an answer. Every guard here is a
#     read, and a failed read returns nothing — which looks exactly like "no
#     rejected files" or "not triggered, optional". So a poll in which ANY
#     call failed decides nothing: it is retried, and a read still failing at
#     the deadline fails the gate as "GitHub API unavailable";
#   * one check name appearing twice on a commit — see "Which check-run
#     decides" below.
#
# It deliberately does NOT create or modify branch protection. The owner
# configures exactly one required check, `ci-required`, per repo.
#
# TWIN: byte-identical in vidra-core, vidra-search, vidra-user and the vidra
# meta repo, and driven by scripts/ci/require-checks_test.sh beside it (also a
# twin). Change all four copies together and run the test.
#
# Env: GH_TOKEN (checks:read + actions:read), GITHUB_REPOSITORY, CHECK_SHA (the
# full commit SHA), DEADLINE_MINUTES, POLL_SECONDS. MAX_POLLS is TEST-ONLY: it
# gives up after that many polls as if the deadline had passed, so the test
# can script an outage without waiting real minutes. CI never sets it.
set -euo pipefail

manifest=${MANIFEST:-.github/required-checks.txt}
repo=${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}
sha=${CHECK_SHA:?CHECK_SHA is required}
deadline_min=${DEADLINE_MINUTES:-75}
interval=${POLL_SECONDS:-20}
max_polls=${MAX_POLLS:-0}

[ -r "$manifest" ] || { echo "::error::ci-required: manifest $manifest is missing" >&2; exit 1; }

# The workflow-runs listing matches head_sha EXACTLY: an abbreviated SHA lists
# zero runs while the check-runs endpoint still resolves it, which would switch
# the zero-job guard off without a word.
grep -Eq '^[0-9a-f]{40}$' <<<"$sha" \
  || { echo "::error::ci-required: CHECK_SHA must be a full 40-character commit SHA, got '${sha}'" >&2; exit 1; }

required=$(grep -vE '^[[:space:]]*(#|$)' "$manifest" | sed 's/[[:space:]]*$//')
[ -n "$required" ] || { echo "::error::ci-required: $manifest lists no checks" >&2; exit 1; }

echo "ci-required: commit ${sha}"
echo "ci-required: manifest ${manifest}"
printf '%s\n' "$required" | sed 's/^/  - /'

gh_err=$(mktemp)
trap 'rm -f "$gh_err"' EXIT

# fetch URL [gh api args...] — one read into $fetched. Any non-zero gh exit
# fails it, with the reason in $api_error. That includes `--paginate` dying on
# a later page AFTER printing the earlier ones: partial output is not an answer.
fetch() {
  local url=$1
  shift
  if fetched=$(gh api -H "Accept: application/vnd.github+json" "$@" "$url" 2>"$gh_err"); then
    return 0
  fi
  fetched=""
  api_error="${url}: $(tr '\n' ' ' <"$gh_err" | cut -c1-300 | sed 's/ *$//')"
  return 1
}

# --- Which check-run decides --------------------------------------------------
# One name can carry several check-runs on one commit. Every workflow run is
# its own check suite, so a re-opened PR, a second event on the same SHA or a
# second release leaves the old runs beside the new ones — vidra-search
# 1f8b8542 has two `publish` runs from two `release` events. (A re-run ATTEMPT
# stays in its suite, where this endpoint's default `filter=latest` already
# shows only the newest attempt — vidra-core 13e5d04.) Taking whichever row the
# API listed first let an older success hide a newer failure. The rule, per name:
#   1. only check-runs created by GitHub Actions count — another app's check
#      that happens to share a lane's name proves nothing about that lane;
#   2. if ANY of them is not completed, wait — the lane is still being decided;
#   3. group them by the workflow FILE that ran them (check suite -> run path);
#      within one file the NEWEST check-run (highest id) decides, so a later
#      run of the same file supersedes an older failure, and a run cancelled
#      by a newer one does not block it;
#   4. every file's newest must succeed. Two different files defining the same
#      job name are two proofs, and a newer success in one must not hide a
#      failure in the other. A check-run whose workflow run is not listed is
#      its own group — stricter, never looser.
# decide NAME prints "missing", "pending <status>", "success" or
# "failed <conclusion> (<file>)[; ...]". Anything else is treated as a failure.
decide() {
  LANE=$1 awk -F'\t' '
    FILENAME == ARGV[1] { if ($2 != "") file[$2] = $3; next }
    $3 != ENVIRON["LANE"] { next }
    {
      seen = 1
      if ($4 != "completed") waiting = ($4 == "" ? "unknown status" : $4)
      key = ($2 in file) ? file[$2] : "check suite " $2
      if (!(key in newest) || $1 + 0 > newest[key] + 0) { newest[key] = $1; state[key] = $5 }
    }
    END {
      if (!seen) { print "missing"; exit }
      if (waiting != "") { print "pending " waiting; exit }
      bad = ""
      for (key in newest) if (state[key] != "success") bad = bad (bad == "" ? "" : "; ") state[key] " (" key ")"
      print (bad == "" ? "success" : "failed " bad)
    }' <(printf '%s\n' "$workflow_runs") <(printf '%s\n' "$check_runs")
}

# poll — one complete read of the commit into $pending/$missing/$failed.
# Returns 1 (reason in $api_error) if any read failed; nothing it gathered may
# then be used. A rejected workflow file exits the script directly.
poll() {
  pending=""
  missing=""
  failed=""
  # `--paginate` on both lists: a missed page would look exactly like a lane
  # that never started. Check-runs are read FIRST so every check-run's workflow
  # run already exists by the time the runs are listed.
  fetch "repos/${repo}/commits/${sha}/check-runs?per_page=100" --paginate \
    --jq '.check_runs[] | select(.app.slug == "github-actions") | [.id, .check_suite.id, .name, .status, (.conclusion // "")] | @tsv' \
    || return 1
  check_runs=$fetched
  fetch "repos/${repo}/actions/runs?head_sha=${sha}&per_page=100" --paginate \
    --jq '.workflow_runs[] | [.id, .check_suite_id, .path, (.conclusion // "")] | @tsv' \
    || return 1
  workflow_runs=$fetched

  # --- a run GitHub could not even start -------------------------------------
  # A workflow whose FILE GitHub rejects (a duplicated key, an action ref that
  # does not resolve) still produces a run: conclusion "failure" (or
  # "startup_failure"), zero jobs — and, the part that matters here, no
  # check-run at all, so the lane simply vanishes from the manifest's point of
  # view. vidra-search's rollback-floor.yml failed exactly like this on every
  # push from 2026-09-08 to 2026-09-10 with this gate green. Zero jobs plus a
  # failed conclusion has no other meaning, so it fails here by name, whether
  # or not the manifest lists the lane.
  local run_id wf_path concl entry optional name verdict
  while IFS=$'\t' read -r run_id _ wf_path concl; do
    case $concl in failure|startup_failure) ;; *) continue ;; esac
    fetch "repos/${repo}/actions/runs/${run_id}/jobs?per_page=1" --jq '.total_count' || return 1
    case $fetched in
      0)
        echo "::error::ci-required: GitHub rejected the workflow file ${wf_path} (run ${run_id} concluded ${concl} with zero jobs — a duplicated key, or an action that does not resolve). Nothing that file defines can run for ${sha}." >&2
        exit 1 ;;
      ''|*[!0-9]*)
        api_error="jobs of run ${run_id}: expected a job count, got '${fetched}'"
        return 1 ;;
    esac
  done <<EOM
$workflow_runs
EOM

  while IFS= read -r entry; do
    [ -n "$entry" ] || continue
    optional=0
    name=$entry
    if [ "${name#\?}" != "$name" ]; then optional=1; name=${name#\?}; fi

    verdict=$(decide "$name") || verdict="unreadable"
    case $verdict in
      success) ;;
      missing)
        if [ "$optional" -eq 1 ]; then
          echo "  (not triggered, optional-if-absent): ${name}"
        else
          missing="${missing}${name}"$'\n'
        fi ;;
      pending\ *) pending="${pending}${name} (${verdict#pending })"$'\n' ;;
      failed\ *) failed="${failed}${name} -> ${verdict#failed }"$'\n' ;;
      *) failed="${failed}${name} -> could not be decided (${verdict})"$'\n' ;;
    esac
  done <<EOM
$required
EOM
}

deadline=$(( $(date +%s) + deadline_min * 60 ))
polls=0
api_failures=0

while :; do
  polls=$((polls + 1))
  api_error=""
  if poll; then
    if [ -n "$failed" ]; then
      echo "::error::ci-required: a required lane did not succeed:" >&2
      printf '%s' "$failed" | sed 's/^/  /' >&2
      exit 1
    fi
    if [ -z "$pending" ] && [ -z "$missing" ]; then
      echo "OK: every required check on ${sha} concluded success."
      exit 0
    fi
  else
    api_failures=$((api_failures + 1))
    echo "warning: a GitHub API read failed, so this poll decides nothing: ${api_error}"
  fi

  now=$(date +%s)
  if [ "$now" -ge "$deadline" ] || { [ "$max_polls" -gt 0 ] && [ "$polls" -ge "$max_polls" ]; }; then
    if [ -n "$api_error" ]; then
      echo "::error::ci-required: GitHub API unavailable — ${api_failures} of ${polls} polls could not read ${sha}, including the last, so nothing is confirmed after ${deadline_min} minutes. Last error: ${api_error}" >&2
      exit 1
    fi
    echo "::error::ci-required: gave up after ${deadline_min} minutes." >&2
    [ -n "$pending" ] && { echo "  still running:" >&2; printf '%s' "$pending" | sed 's/^/    /' >&2; }
    [ -n "$missing" ] && { echo "  never started (a required lane is not triggering for this commit):" >&2; printf '%s' "$missing" | sed 's/^/    /' >&2; }
    exit 1
  fi

  if [ -z "$api_error" ]; then
    [ -n "$pending" ] && printf 'waiting: %s' "$(printf '%s' "$pending" | tr '\n' ' ')"
    [ -n "$missing" ] && printf 'not yet started: %s' "$(printf '%s' "$missing" | tr '\n' ' ')"
    echo
  fi
  sleep "$interval"
done
