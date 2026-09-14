#!/usr/bin/env bash
set -euo pipefail

# Complete serial fixture runs reached 992s on the dogfood host (#2646).
# Share this package-only budget across ordinary, race, and coverage gates;
# individual process/hook deadlines and the default serial workload stay intact.
exec env -u DETENT_API_TOKEN go run ./tools/testgate \
    -parallel 1 -timeout 20m -output tmp/workspace-test-evidence "$@" ./internal/workspace
