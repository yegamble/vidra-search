#!/usr/bin/env bash
# Regression suite for scripts/ci/check-required-manifest-removals.sh: the
# guard that makes retiring a required lane a deliberate, tombstoned edit.
#
# TWIN: byte-identical in vidra-core, vidra-search, vidra-user and the vidra
# meta repo, beside the byte-identical script it drives. Change all four.
#
# Why it exists: .github/required-checks.txt defines "required for merge", and
# until this guard it was the one file no gate watched — a PR whose only
# change deleted a line from it triggered no ci-guard run, ci-required read
# the shortened manifest at the PR head, and the PR merged with a lane
# silently dropped. The guard compares the head's manifest with the base's;
# these cases pin what "removed", "tombstoned" and "base" mean, over stubbed
# base files and over real git history (a shallow clone, as on the runner).
#
# Needs bash (3.2 is enough) and git. Nothing here skips: a missing tool fails.
#
#   bash scripts/ci/check-required-manifest-removals_test.sh
#   REMOVALS_SCRIPT=/other/copy.sh bash scripts/ci/check-required-manifest-removals_test.sh
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
script=${REMOVALS_SCRIPT:-$here/check-required-manifest-removals.sh}
[ -r "$script" ] || { echo "check-required-manifest-removals_test: $script is missing" >&2; exit 1; }
command -v git >/dev/null || { echo "check-required-manifest-removals_test: git is required" >&2; exit 1; }

tmp=$(mktemp -d "${TMPDIR:-/tmp}/manifest-removals-test.XXXXXX")
trap 'rm -rf "$tmp"' EXIT

cases=0 assertions=0 failures=0

# begin TITLE: a fresh case dir with a base manifest listing `a`, `?opt` and
# `e2e (local)`, and a head manifest identical to it.
begin() {
  cases=$((cases + 1))
  title=$1
  FX=$tmp/case-$cases
  mkdir -p "$FX"
  printf '# the base\na\n?opt\ne2e (local)\n' >"$FX/base"
  cp "$FX/base" "$FX/head"
}

# head LINE...: replaces the head manifest with these lines.
head_is() { printf '%s\n' "$@" >"$FX/head"; }

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

# expect RC REGEX [VAR=VALUE...]: run the guard on this case's head against
# its stubbed base; exit status must be RC and the output must match REGEX.
expect() {
  local want=$1 pattern=$2 rc=0
  shift 2
  env -i PATH="$PATH" HOME="$tmp" BASE_MANIFEST="$FX/base" "$@" \
    bash "$script" "$FX/head" >"$FX/out" 2>&1 || rc=$?
  check "$want" "$pattern" "$rc"
}

check() {
  local want=$1 pattern=$2 rc=$3
  if [ "$rc" -ne "$want" ]; then
    record 1 "exit $rc, want $want"
  elif ! grep -Eq -- "$pattern" "$FX/out"; then
    record 1 "output does not match /$pattern/"
  else
    record 0
  fi
}

# --- over a stubbed base file --------------------------------------------------
begin "an unchanged manifest passes"
expect 0 'OK: every lane .* still listed'

begin "removing a required lane with no tombstone fails, naming the lane"
head_is 'a' 'e2e (local)'
expect 1 '^  \?opt$'

begin "removing a lane with a tombstone carrying a reason passes"
head_is 'a' 'e2e (local)' '# retired: opt — folded into a on 2026-09-13'
expect 0 'OK: every lane .* still listed .*1 retired'

begin "the tombstone may name the lane with or without its ? prefix"
head_is 'a' 'e2e (local)' '# retired: ?opt — folded into a'
expect 0 'OK: every lane'

begin "a tombstone may use a plain hyphen before its reason"
head_is 'a' 'e2e (local)' '# retired: opt - folded into a'
expect 0 'OK: every lane'

begin "a matrix leg with spaces and parentheses is matched literally"
head_is 'a' '?opt' 'e2e (s3)'
expect 1 '^  e2e \(local\)$'

begin "the same matrix leg tombstoned passes"
head_is 'a' '?opt' '# retired: e2e (local) — the local leg moved to e2e (s3)'
expect 0 'OK: every lane'

begin "adding a lane passes"
head_is 'a' '?opt' 'e2e (local)' 'govulncheck'
expect 0 'OK: every lane'

begin "demoting a required lane to optional-if-absent is a removal of the required entry"
head_is '?a' '?opt' 'e2e (local)'
expect 1 '^  a \(now \?a: required only if it ran'

begin "the demotion passes with a tombstone for the required entry"
head_is '?a' '?opt' 'e2e (local)' '# retired: a — path-filtered from here on, see a.yml'
expect 0 'OK: every lane'

begin "promoting an optional-if-absent lane to required is not a removal"
head_is 'a' 'opt' 'e2e (local)'
expect 0 'OK: every lane'

begin "a tombstone that already retired its lane on the base is history, not an error"
printf 'a\ne2e (local)\n# retired: opt — folded into a\n' >"$FX/base"
head_is 'a' 'e2e (local)' '# retired: opt — folded into a'
expect 0 'OK: every lane .*0 retired'

begin "a tombstone without a reason is not a tombstone"
head_is 'a' 'e2e (local)' '# retired: opt'
expect 1 'tombstone.*no reason'

begin "a tombstone with an empty reason is not a tombstone"
head_is 'a' 'e2e (local)' '# retired: opt —   '
expect 1 'tombstone.*no reason'

begin "a tombstone for a lane that is still listed fails (it would pre-authorise a later removal)"
head_is 'a' '?opt' 'e2e (local)' '# retired: opt — not yet, but soon'
expect 1 'still listed.*opt'

begin "comments, blank lines and trailing spaces do not count as entries"
head_is '# a comment' '' 'a   ' '?opt' 'e2e (local)' '   '
expect 0 'OK: every lane'

begin "an unreadable base manifest fails"
rm "$FX/base"
expect 1 'base manifest.*missing|is missing'

begin "an unreadable head manifest fails"
rm "$FX/head"
expect 1 'is missing'

# --- over real git history, as on the runner ---------------------------------
# The runner has a depth-1 checkout with `origin` pointing at the repo, and a
# pull_request job knows only GITHUB_BASE_REF. So: a bare origin with `main`,
# a shallow clone of it, and the guard run inside the clone.
g() { git -c init.defaultBranch=main -c user.name=t -c user.email=t@example.invalid -c commit.gpgsign=false "$@"; }

git_case() {  # git_case: $FX/origin (bare, main = base manifest) and $FX/clone (depth 1)
  g init -q --bare "$FX/origin"
  g init -q "$FX/seed"
  mkdir -p "$FX/seed/.github"
  cp "$FX/base" "$FX/seed/.github/required-checks.txt"
  g -C "$FX/seed" add -A
  g -C "$FX/seed" commit -q -m base
  g -C "$FX/seed" push -q "$FX/origin" main
  g clone -q --depth=1 "file://$FX/origin" "$FX/clone" 2>/dev/null
}

# expect_git RC REGEX [VAR=VALUE...]: run the guard inside the clone with its
# working-tree manifest set to this case's head.
expect_git() {
  local want=$1 pattern=$2 rc=0
  shift 2
  cp "$FX/head" "$FX/clone/.github/required-checks.txt"
  ( cd "$FX/clone" && env -i PATH="$PATH" HOME="$tmp" "$@" bash "$script" ) >"$FX/out" 2>&1 || rc=$?
  check "$want" "$pattern" "$rc"
}

begin "pull_request: the base branch's manifest is fetched from origin (shallow clone)"
git_case
head_is 'a' 'e2e (local)'
expect_git 1 '^  \?opt$' GITHUB_EVENT_NAME=pull_request GITHUB_BASE_REF=main

begin "pull_request: the tombstoned removal passes against the fetched base"
git_case
head_is 'a' 'e2e (local)' '# retired: opt — folded into a'
expect_git 0 'OK: every lane origin/main requires' GITHUB_EVENT_NAME=pull_request GITHUB_BASE_REF=main

begin "pull_request: a base branch that cannot be fetched fails, not skips"
git_case
expect_git 1 'could not read' GITHUB_EVENT_NAME=pull_request GITHUB_BASE_REF=no-such-branch

begin "push: the previous commit is the base, deepened from a depth-1 clone"
git_case
head_is 'a' 'e2e (local)'
g -C "$FX/clone" add -A >/dev/null
cp "$FX/head" "$FX/clone/.github/required-checks.txt"
g -C "$FX/clone" commit -q -am "drop opt"
expect_git 1 '^  \?opt$' GITHUB_EVENT_NAME=push

begin "push: a root commit has no parent, so the guard skips and says so"
git_case
# A fresh clone of a one-commit repo: HEAD~1 does not exist anywhere.
expect_git 0 'skip.*no parent' GITHUB_EVENT_NAME=push

begin "no CI variables at all: origin/main is the base"
git_case
head_is 'a' 'e2e (local)'
expect_git 1 '^  \?opt$'

echo "check-required-manifest-removals_test: $cases cases, $assertions assertions, $failures failed"
[ "$failures" -eq 0 ]
