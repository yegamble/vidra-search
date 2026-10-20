#!/usr/bin/env bash
# TWIN: keep this runner identical in vidra-core, vidra-search and vidra-user.
# Versioned Go modules use the checksum database; uv resolves an exact scanner.
set -euo pipefail
cd "$(dirname "$0")/../.."
command -v shellcheck >/dev/null 2>&1 || { echo 'workflow-lint: install ShellCheck before running this gate' >&2; exit 1; }
go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.12
uv tool run --from 'zizmor==1.29.0' zizmor --offline --strict-collection .github/workflows
