#!/usr/bin/env bash
set -euo pipefail

# Workspace tests stub the host-wide process scan (lsof / scratch environment
# inventory) unless a test opts into the real scanner, so the package no longer
# needs a serial run or the 30-minute budget recorded in
# .detent/validation/2962/README.md. Share this package-only budget across
# ordinary, race, and coverage gates.
exec env -u DETENT_API_TOKEN go run ./tools/testgate \
    -timeout 15m -output tmp/workspace-test-evidence "$@" ./internal/workspace
