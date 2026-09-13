#!/usr/bin/env bash
# Retiring a required lane is a deliberate, diff-visible act.
#
# .github/required-checks.txt defines "required for merge", and `ci-required`
# reads it at the PR HEAD. So a PR whose only change deletes a line from it —
# `govulncheck`, `dependency-audit`, any lane — used to merge with one proof
# fewer and nothing red: the deletion is exactly what makes the fan-in stop
# asking, ci-guard's path filter did not include the file, and
# check-required-manifest.sh checks that every listed name exists, not that
# every name that WAS listed still is. The one file that defines "required"
# was the one file no gate watched.
#
# This compares the head's manifest with the base's. An entry present on the
# base and absent at the head fails — unless the head file carries a tombstone
# for it, in the same file, in the same diff, with a reason a reviewer can
# disagree with:
#
#   # retired: <name> — <reason>
#
# Entries are compared as written: turning `x` into `?x` drops a proof too
# (required → required-only-if-it-ran) and needs the tombstone for `x`. The
# tombstone may name the lane with or without its `?`. A tombstone without a
# reason fails, and so does a tombstone for a lane that is still listed — that
# would pre-authorise a later, quieter removal.
#
# Base selection, in order:
#   BASE_MANIFEST=<file>      compare against that file (tests; no git)
#   GITHUB_BASE_REF set       a pull_request: the PR's base branch, fetched
#                             from origin (depth 1 when the checkout is shallow)
#   GITHUB_EVENT_NAME=push    the previous commit, HEAD~1, deepened when the
#                             checkout is shallow; a root commit has none, so
#                             the guard skips and says so
#   otherwise                 origin/main, fetched
# A base that cannot be read FAILS — a guard that cannot see the base has
# nothing to say, and "nothing to say" must not look like "nothing removed".
#
# Exit 0 prints one OK line; exit 1 names every removed lane. No API call and
# no token: the repos are public, so an anonymous fetch of the base suffices.
#
# TWIN: byte-identical in vidra-core, vidra-search, vidra-user and the vidra
# meta repo, driven by check-required-manifest-removals_test.sh beside it
# (also a twin). Change all four copies together and run the test.
set -euo pipefail

manifest=${1:-.github/required-checks.txt}
[ -r "$manifest" ] || { echo "::error::required-manifest removals guard: $manifest is missing" >&2; exit 1; }

die() { echo "::error::required-manifest removals guard: $*" >&2; exit 1; }

# entries — the manifest's entries from stdin, one per line, as written.
entries() { grep -vE '^[[:space:]]*(#|$)' | sed 's/^[[:space:]]*//; s/[[:space:]]*$//' || true; }

trim() { printf '%s' "$1" | sed 's/^[[:space:]]*//; s/[[:space:]]*$//'; }

# listed LIST NAME — NAME is one of LIST's lines, exactly (`?` included).
listed() { printf '%s\n' "$1" | grep -qxF -- "$2"; }

shallow() { [ "$(git rev-parse --is-shallow-repository 2>/dev/null)" = true ]; }

# fetch_show REF — the base's manifest as of origin/REF, via FETCH_HEAD so a
# depth-1 checkout (the runner's) never needs the branch locally.
fetch_show() {
  if shallow; then
    git fetch -q --depth=1 origin "$1" || return 1
  else
    git fetch -q origin "$1" || return 1
  fi
  git show "FETCH_HEAD:${manifest}"
}

if [ -n "${BASE_MANIFEST:-}" ]; then
  [ -r "$BASE_MANIFEST" ] || die "base manifest $BASE_MANIFEST is missing"
  base_label=$BASE_MANIFEST
  base_text=$(cat "$BASE_MANIFEST")
elif [ -n "${GITHUB_BASE_REF:-}" ]; then
  base_label="origin/${GITHUB_BASE_REF}"
  base_text=$(fetch_show "$GITHUB_BASE_REF") \
    || die "could not read ${manifest} from ${base_label} (is the base branch fetchable from origin?)"
elif [ "${GITHUB_EVENT_NAME:-}" = push ]; then
  if ! git rev-parse -q --verify 'HEAD~1^{commit}' >/dev/null 2>&1 && shallow; then
    git fetch -q --deepen=1 origin "$(git rev-parse HEAD)" 2>/dev/null || true
  fi
  if ! git rev-parse -q --verify 'HEAD~1^{commit}' >/dev/null 2>&1; then
    echo "skip: HEAD has no parent commit to compare ${manifest} against (a root commit), so nothing can have been removed here."
    exit 0
  fi
  base_label="HEAD~1 ($(git rev-parse --short HEAD~1))"
  # A manifest that did not exist on the parent has nothing to lose.
  base_text=$(git show "HEAD~1:${manifest}" 2>/dev/null || true)
else
  base_label=origin/main
  base_text=$(fetch_show main) \
    || die "could not read ${manifest} from ${base_label} (is origin reachable?)"
fi

base_entries=$(printf '%s\n' "$base_text" | entries)
head_entries=$(entries <"$manifest")

# --- tombstones ---------------------------------------------------------------
# `# retired: <name> — <reason>`: the name runs up to the first " — " (em
# dash), " -- " or " - "; both name and reason must be non-empty.
tombstoned=""
tombstone_errors=""
while IFS= read -r line; do
  [ -n "$line" ] || continue
  rest=${line#*retired:}
  name=$rest
  reason=""
  for sep in ' — ' ' -- ' ' - '; do
    case $rest in
      *"$sep"*) name=${rest%%"$sep"*}; reason=${rest#*"$sep"}; break ;;
    esac
  done
  name=$(trim "$name")
  name=${name#\?}
  reason=$(trim "$reason")
  if [ -z "$name" ] || [ -z "$reason" ]; then
    tombstone_errors="${tombstone_errors}  tombstone with no reason: ${line}"$'\n'
    continue
  fi
  # Still listed at the head, as required, or as `?name` when it was `?name`
  # (or nothing) on the base too: then it retires nothing. `name` -> `?name`
  # with the tombstone for `name` is the one listed form that is a retirement.
  if listed "$head_entries" "$name"; then
    tombstone_errors="${tombstone_errors}  tombstone for a lane that is still listed: ${name} (retire it in the same edit, or drop the tombstone)"$'\n'
    continue
  fi
  if listed "$head_entries" "?${name}" && ! listed "$base_entries" "$name"; then
    tombstone_errors="${tombstone_errors}  tombstone for a lane that is still listed: ?${name} (retire it in the same edit, or drop the tombstone)"$'\n'
    continue
  fi
  tombstoned="${tombstoned}${name}"$'\n'
done <<EOM
$(grep -E '^[[:space:]]*#[[:space:]]*retired:' "$manifest" || true)
EOM

# --- removals -----------------------------------------------------------------
removed=""
retired=0
while IFS= read -r entry; do
  [ -n "$entry" ] || continue
  listed "$head_entries" "$entry" && continue
  name=${entry#\?}
  # `?name` -> `name` is a promotion: stricter, not a removal.
  case $entry in \?*) listed "$head_entries" "$name" && continue ;; esac
  if listed "$tombstoned" "$name"; then
    retired=$((retired + 1))
    continue
  fi
  if listed "$head_entries" "?${entry}"; then
    removed="${removed}  ${entry} (now ?${entry}: required only if it ran, which is a proof dropped)"$'\n'
  else
    removed="${removed}  ${entry}"$'\n'
  fi
done <<EOM
$base_entries
EOM

if [ -n "$tombstone_errors" ]; then
  echo "::error::$(basename "$manifest") carries a tombstone that does not retire anything:" >&2
  printf '%s' "$tombstone_errors" >&2
  exit 1
fi
if [ -n "$removed" ]; then
  echo "::error::$(basename "$manifest") no longer lists $(printf '%s' "$removed" | grep -c .) lane(s) that ${base_label} requires:" >&2
  printf '%s' "$removed" >&2
  echo "Retiring a lane is deliberate: add '# retired: <name> — <reason>' to ${manifest} in this same change, so the removal and its reason are one reviewable diff." >&2
  exit 1
fi
echo "OK: every lane ${base_label} requires is still listed in ${manifest} ($(printf '%s\n' "$head_entries" | grep -c .) entries; ${retired} retired with a reason)."
