#!/usr/bin/env bash
set -euo pipefail

# Serial runs exhausted 1200s with only five subtests left on the dogfood host
# (#2962); allow 50% headroom over that observed cumulative workload.
# Timing evidence and fixture analysis: .detent/validation/2962/README.md.
# Share this package-only budget across ordinary, race, and coverage gates;
# individual process/hook deadlines and the default serial workload stay intact.
exec env -u DETENT_API_TOKEN go run ./tools/testgate \
    -parallel 1 -timeout 30m -output tmp/workspace-test-evidence "$@" ./internal/workspace
