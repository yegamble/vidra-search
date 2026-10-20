#!/usr/bin/env bash
# Audit the locked training graph, not the scanner's own environment.
set -euo pipefail
cd "$(dirname "$0")/../.."
requirements=$(mktemp)
trap 'rm -f "$requirements"' EXIT
uv export --directory training --locked --all-extras --no-hashes --no-emit-project -o "$requirements"
uv tool run --python 3.11 --from 'pip-audit==2.10.1' pip-audit \
  --strict --no-deps --disable-pip -r "$requirements"
