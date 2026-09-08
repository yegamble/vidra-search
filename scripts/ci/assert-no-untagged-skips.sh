#!/usr/bin/env bash
# Static half of the A39 / QLT-01 no-silent-skip gate, for the CANONICAL unit
# gate (`make ci` -> test-race, i.e. the default build with no build tags).
#
# The integration lanes get a RUNTIME audit (assert-no-silent-skips.sh over a
# `go test -v` log) because their skips are environment-conditional. The unit
# gate cannot be audited that way without running the whole suite a second time
# — `make ci` deliberately runs `go test` non-verbosely, where a skip prints
# nothing at all. So the unit gate is guarded STATICALLY instead: a test in the
# default build may only skip for a reason this repo has registered.
#
# What it stops: someone adding `if os.Getenv("X") == "" { t.Skip(...) }` (or a
# `testing.Short()` bail) to an UNTAGGED test file. That test would then be part
# of the required gate in name only — green on every machine that lacks X,
# including CI. Tagged integration files are exempt: self-skipping is their
# documented design, and their lanes assert at runtime instead.
#
# Usage: assert-no-untagged-skips.sh [allowlist]   (run from the repo root)
set -euo pipefail

allow=${1:-scripts/ci/allowed-skips-unit.txt}
[ -r "$allow" ] || { echo "::error::untagged-skip guard: cannot read allowlist $allow" >&2; exit 1; }

patterns=$(grep -vE '^[[:space:]]*(#|$)' "$allow" || true)

status=0
found=0
while IFS= read -r f; do
  # Files carrying a CUSTOM build tag (integration, ipfs_integration, …) are not
  # part of the default build and are audited at runtime by their own lane.
  # `//go:build unix` and friends are platform constraints, not opt-in tags, so
  # those files DO compile into the canonical gate and stay in scope here.
  if grep -qE '^//go:build .*(integration|e2e|manual)' "$f"; then
    continue
  fi
  while IFS= read -r hit; do
    [ -n "$hit" ] || continue
    found=$((found + 1))
    # `file:line:  t.Skip("reason")` -> reason (best effort; a non-literal
    # argument yields the raw call, which will not match and so is reported).
    reason=$(printf '%s' "$hit" | sed -E 's/^[0-9]+:[[:space:]]*//; s/^.*t\.Skipf?\(//; s/^"//; s/".*$//')
    ok=0
    if [ -n "$patterns" ]; then
      while IFS= read -r p; do
        [ -n "$p" ] || continue
        if printf '%s' "$reason" | grep -qE -- "$p"; then ok=1; break; fi
      done <<EOP
$patterns
EOP
    fi
    if [ "$ok" -eq 0 ]; then
      status=1
      echo "::error file=${f#./},line=${hit%%:*}::untagged-skip guard: a test in the DEFAULT build skips itself: ${f#./}:${hit}" >&2
    fi
  done <<EOH
$(grep -nE '(^|[^[:alnum:]_])t\.Skipf?\(|testing\.Short\(\)' "$f" || true)
EOH
done <<EOF2
$(find . -name '*_test.go' -not -path './vendor/*' | sort)
EOF2

if [ "$status" -ne 0 ]; then
  echo "::error::untagged-skip guard failed. Either move the test behind a build tag with its own CI lane, make the dependency unconditional, or register the reason in ${allow}." >&2
  exit 1
fi
echo "OK: untagged-skip guard — ${found} skip site(s) in the default build, all registered in $(basename "$allow")."
