#!/usr/bin/env bash
# TWIN: vidra-core and vidra-search. Pass each CI build-tag world separately;
# an empty argument is the default build. Tests remain analysis roots.
set -euo pipefail
cd "$(dirname "$0")/../.."
staticcheck='honnef.co/go/tools/cmd/staticcheck@v0.8.2'
go run "$staticcheck" -version
if [ "$#" -eq 0 ]; then set -- ''; fi
for tags in "$@"; do
  printf 'Staticcheck build tags: %s (tests included)\n' "${tags:-default}"
  go run "$staticcheck" -tests=true -tags="$tags" ./...
done
