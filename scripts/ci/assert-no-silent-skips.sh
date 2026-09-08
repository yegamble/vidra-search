#!/usr/bin/env bash
# Audit a `go test -v` log for tests that skipped themselves.
#
# WHY THIS EXISTS (A39 / QLT-01). Nearly every integration test in this repo is
# written to self-skip when its dependency is absent:
#
#     if os.Getenv("DATABASE_URL") == "" {
#         t.Skip("DATABASE_URL not set; skipping integration test")
#     }
#
# That is the right behaviour on a developer laptop and the WRONG behaviour in
# the lane whose entire job is to prove those paths work: if a service fails to
# start, an env var is renamed, or a `services:` block is dropped from the
# workflow, `go test` exits 0 having executed nothing and the check goes green.
# The clamav and MinIO comments in backend-integration.yml already name that
# failure mode; this script is the assertion that closes it.
#
# Contract: every `--- SKIP` in LOG must have a reason matching a line in
# ALLOWLIST (an extended-regex per line, `#` comments and blanks ignored).
# Anything else fails the job, naming the test and the reason. The allowlist is
# therefore the checked-in register of skips the team has explicitly accepted —
# adding one is a reviewed edit, not a silent green.
#
# Also fails when the log contains no passing test at all: a lane that compiled
# everything and ran nothing must not look like a lane that proved something.
#
# Usage: assert-no-silent-skips.sh <go-test-verbose-log> <allowlist-file>
set -euo pipefail

log=${1:?usage: assert-no-silent-skips.sh <log> <allowlist>}
allow=${2:?usage: assert-no-silent-skips.sh <log> <allowlist>}

[ -r "$log" ] || { echo "::error::skip audit: cannot read test log $log" >&2; exit 1; }
[ -r "$allow" ] || { echo "::error::skip audit: cannot read allowlist $allow" >&2; exit 1; }

# Pair each `--- SKIP: <name>` with the nearest preceding `<file>_test.go:NN: <reason>`
# line, which is where t.Skip's message lands. Subtests indent both lines, so the
# patterns are anchored loosely on purpose.
skips=$(awk '
  /_test\.go:[0-9]+: / { sub(/^.*_test\.go:[0-9]+: /, ""); reason = $0; next }
  /^[[:space:]]*--- SKIP: / {
    name = $0; sub(/^[[:space:]]*--- SKIP: /, "", name); sub(/ \(.*$/, "", name)
    printf "%s\t%s\n", name, (reason == "" ? "(no reason recorded)" : reason)
    reason = ""
  }
' "$log")

passed=$(grep -cE '^[[:space:]]*--- PASS: ' "$log" || true)
if [ "${passed:-0}" -eq 0 ]; then
  echo "::error::skip audit: $log records zero passing tests — this lane proved nothing." >&2
  exit 1
fi

if [ -z "$skips" ]; then
  echo "OK: skip audit — 0 skipped, ${passed} passed in $(basename "$log")."
  exit 0
fi

# Strip comments/blank lines; an empty allowlist means "no skip is acceptable here".
patterns=$(grep -vE '^[[:space:]]*(#|$)' "$allow" || true)

status=0
allowed=0
while IFS=$'\t' read -r name reason; do
  [ -n "$name" ] || continue
  ok=0
  if [ -n "$patterns" ]; then
    while IFS= read -r p; do
      [ -n "$p" ] || continue
      if printf '%s' "$reason" | grep -qE -- "$p"; then ok=1; break; fi
    done <<EOP
$patterns
EOP
  fi
  if [ "$ok" -eq 1 ]; then
    allowed=$((allowed + 1))
    echo "  allowed skip: ${name} — ${reason}"
  else
    status=1
    echo "::error::skip audit: ${name} SKIPPED with an unregistered reason: ${reason}" >&2
  fi
done <<EOS
$skips
EOS

total=$(printf '%s\n' "$skips" | grep -c . || true)
if [ "$status" -ne 0 ]; then
  echo "::error::skip audit failed: $((total - allowed)) of ${total} skips in $(basename "$log") are not in $(basename "$allow"). Either provide the missing dependency in the workflow, or register the skip in the allowlist with a reason." >&2
  exit 1
fi

echo "OK: skip audit — ${allowed} registered skips, ${passed} passed, 0 unregistered in $(basename "$log")."
