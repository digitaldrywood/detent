#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
node scripts/release-smoke.mjs "$@" | go run ./tools/releasesmoke "$@"
