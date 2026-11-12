#!/usr/bin/env bash
set -euo pipefail

exec env -u DETENT_API_TOKEN go run ./tools/testgate \
    -timeout 10m -output tmp/workspace-test-evidence "$@" ./internal/workspace/...
