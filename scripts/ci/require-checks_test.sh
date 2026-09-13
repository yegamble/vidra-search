#!/usr/bin/env bash
# Regression suite for scripts/ci/require-checks.sh, the `ci-required` fan-in.
#
# TWIN: byte-identical in vidra-core, vidra-search, vidra-user and the vidra
# meta repo, beside the byte-identical script it drives. Change all four.
#
# Why it exists: the fan-in is the ONE check branch protection trusts, so every
# way it can mis-read the API is a way for a PR to merge with a proof missing.
# A QA review drove it with a stub `gh` and found it exiting 0 when the reads
# behind its guards failed, when a paginated read died half way, when a
# zero-job run concluded `startup_failure`, and when a lane had two check-runs
# of one name (whichever row came first won). Nothing re-ran those scenarios,
# so any later edit could have reopened them with every check green.
#
# How: a stub `gh`, first on PATH, answers each call from RAW JSON fixtures and
# applies the script's own `--jq` filter with the real jq, so the filters are
# under test and not only the shell around them. A fixture file may hold
# several JSON documents, one per page: with `--paginate` the filter runs over
# every page, without it only the first page is seen, as with the real gh.
# `<kind>.<n>` answers only the n-th call of a kind, `<kind>` every other one;
# a matching `.rc` file sets the exit code (with an HTTP-ish error on stderr,
# or the text of a matching `.err` file), and `<kind>.pages` stops output after
# that many pages, like a paginated read that dies part way.
#
# Needs bash (3.2 is enough) and jq. Nothing here skips: a missing tool fails.
#
#   bash scripts/ci/require-checks_test.sh
#   REQUIRE_CHECKS_SCRIPT=/other/copy.sh bash scripts/ci/require-checks_test.sh
#
# The override runs the suite against another copy, e.g. an older revision to
# show which cases it fails.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
script=${REQUIRE_CHECKS_SCRIPT:-$here/require-checks.sh}
[ -r "$script" ] || { echo "require-checks_test: $script is missing" >&2; exit 1; }
command -v jq >/dev/null || { echo "require-checks_test: jq is required (the stub applies the script's --jq filters with it)" >&2; exit 1; }

tmp=$(mktemp -d "${TMPDIR:-/tmp}/require-checks-test.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
mkdir "$tmp/bin"

cat >"$tmp/bin/gh" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
[ "${1:-}" = api ] || { echo "stub gh: only 'gh api' is stubbed: $*" >&2; exit 97; }
shift
url="" expr="." paginate=0
while [ $# -gt 0 ]; do
  case $1 in
    --paginate) paginate=1 ;;
    -H) shift ;;
    --jq) shift; expr=$1 ;;
    repos/*|rate_limit) url=$1 ;;
    *) echo "stub gh: unexpected argument: $1" >&2; exit 97 ;;
  esac
  shift
done
case $url in
  */commits/*/check-runs\?*) kind=checks ;;
  */actions/runs/*/jobs\?*) run=${url#*/actions/runs/}; kind=jobs-${run%%/*} ;;
  */actions/runs\?head_sha=*) kind=runs ;;
  rate_limit) kind=ratelimit ;;
  *) echo "stub gh: no fixture kind for $url" >&2; exit 97 ;;
esac
n=$(( $(cat "$FX/.calls-$kind" 2>/dev/null || echo 0) + 1 ))
echo "$n" >"$FX/.calls-$kind"
# A script that ignores MAX_POLLS would spin until its real deadline: stop it.
if [ "$n" -gt 20 ]; then
  echo "stub gh: more than 20 '$kind' calls; killing the script under test" >>"$FX/out"
  kill "$(cat "$FX/pid")"
  exit 97
fi
body=$FX/$kind.$n; [ -e "$body" ] || body=$FX/$kind
pages=1000000; [ -e "$FX/$kind.pages" ] && pages=$(cat "$FX/$kind.pages")
rc=0 err="gh: Server Error (HTTP 502)"
if [ -e "$FX/$kind.$n.rc" ]; then rc=$(cat "$FX/$kind.$n.rc"); elif [ -e "$FX/$kind.rc" ]; then rc=$(cat "$FX/$kind.rc"); fi
if [ -e "$FX/$kind.$n.err" ]; then err=$(cat "$FX/$kind.$n.err"); elif [ -e "$FX/$kind.err" ]; then err=$(cat "$FX/$kind.err"); fi
if [ -e "$body" ]; then
  if [ "$paginate" -eq 1 ]; then
    jq -r -n "limit($pages; inputs) | ($expr)" "$body" || exit 98
  else
    jq -r -n "input | ($expr)" "$body" || exit 98
  fi
elif [ "$rc" -eq 0 ]; then
  echo "stub gh: no fixture $body" >&2
  exit 97
fi
[ "$rc" -eq 0 ] || echo "$err" >&2
exit "$rc"
STUB
chmod +x "$tmp/bin/gh"

SHA=0123456789abcdef0123456789abcdef01234567
A=.github/workflows/a.yml
OPT=.github/workflows/opt.yml
UNLISTED=.github/workflows/unlisted.yml
OTHER=.github/workflows/other.yml
SELF=.github/workflows/ci-required.yml

cases=0 assertions=0 failures=0

# begin TITLE: a fresh fixture dir. Manifest: `a` required, `?opt` optional.
# Default workflow runs map check suite 10 -> a.yml, 20 -> opt.yml.
begin() {
  cases=$((cases + 1))
  title=$1
  FX=$tmp/case-$cases
  mkdir "$FX"
  printf 'a\n?opt\n' >"$FX/manifest"
  runs "1|10|$A|success" "2|20|$OPT|success"
}

# checks ROW... appends ONE page of check-runs. ROW: id|suite|name|status|conclusion[|app]
# With POLL=n set, the page answers only the n-th check-runs read.
checks() {
  printf '%s\n' "$@" | jq -Rn '[inputs | select(length > 0) | split("|") | {
      id: (.[0] | tonumber), check_suite: {id: (.[1] | tonumber)}, name: .[2],
      status: .[3], conclusion: (if .[4] == "" then null else .[4] end),
      app: {slug: (.[5] // "github-actions")}}]
    | {total_count: length, check_runs: .}' >>"$FX/checks${POLL:+.$POLL}"
}

# runs ROW... replaces the workflow-runs list. ROW: id|suite|path|conclusion[|status]
# (an empty conclusion means the run is still going: `in_progress` unless given)
# With POLL=n set, the list answers only the n-th workflow-runs read.
runs() {
  printf '%s\n' "$@" | jq -Rn '[inputs | select(length > 0) | split("|") | {
      id: (.[0] | tonumber), check_suite_id: (.[1] | tonumber), path: .[2], run_attempt: 1,
      status: (if .[3] != "" then "completed" else (.[4] // "in_progress") end),
      conclusion: (if .[3] == "" then null else .[3] end)}]
    | {total_count: length, workflow_runs: .}' >"$FX/runs${POLL:+.$POLL}"
}

# jobcount RUN_ID COUNT: that run's jobs listing.
jobcount() { printf '{"total_count":%s,"jobs":[]}\n' "$2" >"$FX/jobs-$1"; }

# fail KIND [N]: the N-th call of KIND (every call without N) exits 1.
# (`echo P >"$FX/KIND.pages"` makes it print only the first P pages first.)
fail() { echo 1 >"$FX/$1${2:+.$2}.rc"; }

# ratelimited KIND [N]: the N-th call of KIND (every call without N) fails the
# way gh reports an exhausted primary rate limit (HTTP 403).
ratelimited() {
  fail "$@"
  echo "gh: API rate limit exceeded for installation ID 1. If you reach out to GitHub Support for help, please include the request ID 0000. (HTTP 403)" >"$FX/$1${2:+.$2}.err"
}

# reset_at EPOCH: what the rate_limit endpoint reports as the core reset time.
reset_at() { printf '{"resources":{"core":{"limit":1000,"remaining":0,"reset":%s}}}\n' "$1" >"$FX/ratelimit"; }

record() {
  assertions=$((assertions + 1))
  if [ "$1" -eq 0 ]; then
    echo "ok $assertions - $title"
  else
    failures=$((failures + 1))
    echo "not ok $assertions - $title: $2"
    if [ -r "$FX/out" ]; then sed 's/^/    # /' "$FX/out"; fi
  fi
}

# expect RC REGEX [VAR=VALUE...]: run the script on this case's fixtures; its
# exit status must be RC and its combined output must match REGEX.
expect() {
  local want=$1 pattern=$2 rc=0 pid
  shift 2
  env PATH="$tmp/bin:$PATH" FX="$FX" GITHUB_REPOSITORY=o/r CHECK_SHA="$SHA" \
    MANIFEST="$FX/manifest" DEADLINE_MINUTES=0 POLL_SECONDS=0 MAX_POLLS=0 "$@" \
    bash "$script" >"$FX/out" 2>&1 &
  pid=$!
  echo "$pid" >"$FX/pid"
  wait "$pid" || rc=$?
  if [ "$rc" -ne "$want" ]; then
    record 1 "exit $rc, want $want"
  elif ! grep -Eq -- "$pattern" "$FX/out"; then
    record 1 "output does not match /$pattern/"
  else
    record 0
  fi
}

# expect_calls KIND N: the script made exactly N calls of KIND (proves retries).
expect_calls() {
  local got
  got=$(cat "$FX/.calls-$1" 2>/dev/null || echo 0)
  title="$title ($2 '$1' reads)"
  if [ "$got" -eq "$2" ]; then record 0; else record 1 "$got '$1' reads, want $2"; fi
}

# --- verdicts that were already fail-closed stay fail-closed ------------------
begin "every required lane succeeded"
checks "100|10|a|completed|success"
expect 0 'OK: every required check'

begin "a required lane that never ran fails"
checks "200|20|opt|completed|success"
expect 1 'never started'

begin "an optional lane that did not run passes"
checks "100|10|a|completed|success"
expect 0 'optional-if-absent\): opt'

# The gate cannot tell which file an absent lane belongs to, so while ANY run
# on the commit is still queued or running, a `?lane` with no check-run yet is
# pending, not absent — its job may be seconds from being created. It converges
# because every run completes.
begin "an optional lane whose workflow run is still queued is pending, not absent"
runs "1|10|$A|success" "2|20|$OPT||queued"
checks "100|10|a|completed|success"
expect 1 'still running'

begin "an optional lane is absent only once every run on the SHA has completed"
POLL=1 runs "1|10|$A|" "2|20|$OPT||queued"
POLL=1 checks "100|10|a|in_progress|"
POLL=2 runs "1|10|$A|success" "2|20|$OPT|success"
POLL=2 checks "100|10|a|completed|success"
expect 0 'optional-if-absent\): opt' DEADLINE_MINUTES=5 MAX_POLLS=3
expect_calls checks 2

# The one run that is in progress for as long as the gate polls is the gate's
# own. Counting it would hold every absent `?lane` until the deadline.
begin "the gate's own run (by id) does not hold an absent optional lane"
runs "1|10|$A|success" "9|90|$SELF||in_progress"
checks "100|10|a|completed|success"
expect 0 'optional-if-absent\): opt' GITHUB_RUN_ID=9

begin "another run of the gate's own workflow (by file) does not hold an absent optional lane"
runs "1|10|$A|success" "9|90|$SELF||in_progress"
checks "100|10|a|completed|success"
expect 0 'optional-if-absent\): opt' GITHUB_RUN_ID=8 GITHUB_WORKFLOW_REF="o/r/$SELF@refs/pull/1/merge"

for c in skipped cancelled neutral timed_out failure; do
  begin "a required lane concluding $c fails"
  checks "100|10|a|completed|$c"
  expect 1 "a -> $c"

  begin "an optional lane that ran and concluded $c fails"
  checks "100|10|a|completed|success" "200|20|opt|completed|$c"
  expect 1 "opt -> $c"
done

begin "a lane still running at the deadline fails"
checks "100|10|a|in_progress|"
expect 1 'still running'

begin "the lane on the second page of check-runs is found"
checks "200|20|opt|completed|success"
checks "100|10|a|completed|success"
expect 0 'OK: every required check'

# --- a workflow file GitHub rejected: zero jobs, no check-run at all ----------
for c in failure startup_failure; do
  begin "a zero-job $c in an optional lane's file fails"
  runs "1|10|$A|success" "2|20|$OPT|$c"
  jobcount 2 0
  checks "100|10|a|completed|success"
  expect 1 "rejected the workflow file $OPT"

  begin "a zero-job $c in a file the manifest does not list fails"
  runs "1|10|$A|success" "3|30|$UNLISTED|$c"
  jobcount 3 0
  checks "100|10|a|completed|success"
  expect 1 "rejected the workflow file $UNLISTED"
done

begin "a failed unlisted workflow that did run jobs is not a rejected file"
runs "1|10|$A|success" "3|30|$UNLISTED|failure"
jobcount 3 4
checks "100|10|a|completed|success"
expect 0 'OK: every required check'

begin "a finished run's job count is read once, not on every poll"
runs "1|10|$A|" "3|30|$UNLISTED|failure"
jobcount 3 4
checks "100|10|a|in_progress|"
expect 1 'still running' DEADLINE_MINUTES=5 MAX_POLLS=3
expect_calls jobs-3 1

# --- an API read that fails decides nothing ----------------------------------
begin "the jobs-count read failing does not pass"
runs "1|10|$A|success" "3|30|$UNLISTED|failure"
fail jobs-3
checks "100|10|a|completed|success"
expect 1 'GitHub API unavailable'

begin "the workflow-runs read failing does not pass"
fail runs
checks "100|10|a|completed|success"
expect 1 'GitHub API unavailable'

begin "a paginated read that dies after page one does not pass"
runs "1|10|$A|success" "4|11|$A|failure"
jobcount 4 3
checks "100|10|a|completed|success"
checks "101|11|a|completed|failure"
echo 1 >"$FX/checks.pages"
fail checks
expect 1 'GitHub API unavailable'

begin "a check-runs read that fails once is retried, then passes"
checks "100|10|a|completed|success"
fail checks 1
expect 0 'OK: every required check' DEADLINE_MINUTES=5 MAX_POLLS=3
expect_calls checks 2

begin "check-runs failing on every poll fails, naming the API"
fail checks
expect 1 'GitHub API unavailable.*HTTP 502' DEADLINE_MINUTES=5 MAX_POLLS=3
expect_calls checks 3

begin "an outage spanning two polls that then clears is survived"
checks "100|10|a|completed|success"
fail checks 1
fail checks 2
expect 0 'OK: every required check' DEADLINE_MINUTES=5 MAX_POLLS=3
expect_calls checks 3

# --- the token's own rate limit is not an outage ------------------------------
# GITHUB_TOKEN gets 1,000 requests an hour PER REPOSITORY, shared by every job
# running there. A 403 saying so is this gate's own budget, spent by its
# siblings: it is waited out until the reset, not counted as a failed read.
begin "a rate-limited read waits for the reset instead of counting as an outage"
checks "100|10|a|completed|success"
ratelimited checks 1
ratelimited checks 2
reset_at "$(( $(date +%s) + 1 ))"
expect 0 'waiting [0-9]+s for the rate limit to reset' DEADLINE_MINUTES=5 MAX_POLLS=3
expect_calls checks 3
expect_calls ratelimit 2

begin "a rate limit still in force at the deadline fails, and says so"
checks "100|10|a|completed|success"
ratelimited checks
reset_at "$(( $(date +%s) + 3600 ))"
expect 1 'GitHub API unavailable.*rate-limited'

# --- two check-runs with one name ---------------------------------------------
begin "one lane twice, older success and newer failure, fails"
runs "1|10|$A|success" "4|11|$A|failure"
jobcount 4 3
checks "100|10|a|completed|success" "101|11|a|completed|failure"
expect 1 'a -> failure'

begin "one lane twice, older failure and newer success (a later run), passes"
runs "1|10|$A|failure" "4|11|$A|success"
jobcount 1 3
checks "100|10|a|completed|failure" "101|11|a|completed|success"
expect 0 'OK: every required check'

begin "one lane twice, the newer still running, waits"
runs "1|10|$A|success" "4|11|$A|"
checks "100|10|a|completed|success" "101|11|a|in_progress|"
expect 1 'still running'

begin "one lane twice, the older still running, still waits"
runs "1|10|$A|" "4|11|$A|success"
checks "100|10|a|in_progress|" "101|11|a|completed|success"
expect 1 'still running'

begin "a re-run attempt of an older run does not supersede a newer run's failure"
runs "1|10|$A|success" "4|11|$A|failure"
jobcount 4 3
checks "150|10|a|completed|success" "101|11|a|completed|failure"
expect 1 'a -> failure'

begin "a newer run of the lane's file still queued waits"
runs "1|10|$A|failure" "4|11|$A||queued"
jobcount 1 3
checks "100|10|a|completed|failure"
expect 1 'still running'

# Manifest without the `?opt` lane: an absent optional lane would, by design,
# wait for this still-running run (see "pending, not absent" above). The
# point here is that `a`'s own verdict is not held by its run's other jobs.
begin "a lane that finished while other jobs of its run still run is decided"
printf 'a\n' >"$FX/manifest"
runs "1|10|$A|"
checks "100|10|a|completed|success"
expect 0 'OK: every required check'

begin "a newer run of the file that ended without the lane does not hide the older result"
runs "1|10|$A|failure" "4|11|$A|cancelled"
jobcount 1 3
checks "100|10|a|completed|failure"
expect 1 'a -> failure'

begin "one name in two workflow files needs both files to succeed"
runs "1|10|$A|failure" "5|50|$OTHER|success"
jobcount 1 3
checks "100|10|a|completed|failure" "101|50|a|completed|success"
expect 1 "a -> failure \\($A\\)"

begin "a check-run whose workflow run is not listed stands on its own"
checks "100|77|a|completed|failure" "101|10|a|completed|success"
expect 1 'a -> failure \(check suite 77\)'

begin "another app's check-run of the same name does not count"
checks "100|90|a|completed|success|some-other-app"
expect 1 'never started'

begin "an abbreviated commit SHA is refused"
checks "100|10|a|completed|success"
expect 1 'full 40-character' CHECK_SHA=0123456

echo "require-checks_test: $cases cases, $assertions assertions, $failures failed"
[ "$failures" -eq 0 ]
