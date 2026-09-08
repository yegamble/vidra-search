#!/usr/bin/env bash
# Assert every check name in .github/required-checks.txt is a job some workflow
# actually defines (A39 / QLT-01).
#
# Without this, renaming or deleting a job leaves `ci-required` waiting for a
# check that can never appear — and the failure arrives more than an hour later
# as an opaque fan-in timeout, on a PR that has nothing to do with the rename.
# This turns that into a ten-second, specific failure in ci-guard.
#
# A matrix leg reports as `<job name> (<leg>)`, which no file contains
# literally, so a parenthesised suffix is stripped before matching the base name
# against the job id or its `name:`.
#
# TWIN: the same script in vidra-core, vidra-search and vidra-user. Keep in step.
set -euo pipefail

manifest=${1:-.github/required-checks.txt}
[ -r "$manifest" ] || { echo "::error::required-manifest guard: $manifest is missing" >&2; exit 1; }

missing=""
count=0
while IFS= read -r raw; do
  name=${raw%%#*}
  name=$(printf '%s' "$name" | sed 's/^[[:space:]]*//; s/[[:space:]]*$//; s/^?//')
  [ -n "$name" ] || continue
  count=$((count + 1))
  base=$(printf '%s' "$name" | sed -E 's/ \(.*\)$//')
  if grep -rqE "^[[:space:]]{2}${base}:|name: ${base}([[:space:]]|\(|\$)" .github/workflows; then
    continue
  fi
  missing="${missing}${name}"$'\n'
done < "$manifest"

if [ -n "$missing" ]; then
  echo "::error::$(basename "$manifest") names checks no workflow defines (renamed or deleted job?):" >&2
  printf '%s' "$missing" | sed 's/^/  /' >&2
  exit 1
fi
echo "OK: all ${count} entries in ${manifest} map to a defined job."
