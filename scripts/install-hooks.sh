#!/usr/bin/env bash
# TWIN: keep this installer identical across the three component repositories.
set -euo pipefail
cd "$(dirname "$0")/.."
if git config --get core.hooksPath >/dev/null; then
  echo 'hooks-install: existing core.hooksPath; integrate Lefthook with that setup first' >&2
  exit 1
fi
hooks=$(git rev-parse --git-path hooks)
for name in pre-commit pre-push; do
  if { [ -e "$hooks/$name" ] || [ -L "$hooks/$name" ]; } && ! grep -q '^call_lefthook()$' "$hooks/$name"; then
    echo "hooks-install: preserving existing $name hook; integrate it with Lefthook first" >&2
    exit 1
  fi
done
# lefthook.yml embeds this pinned command, so installed hooks do not depend on
# an ephemeral `go run` executable or a globally installed unpinned binary.
go run github.com/evilmartians/lefthook/v2@v2.2.2 install
