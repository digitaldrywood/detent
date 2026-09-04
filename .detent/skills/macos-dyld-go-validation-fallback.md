---
name: macos-dyld-go-validation-fallback
description: Diagnose Go validation commands that compile successfully on macOS but hang before the Go runtime starts, then run the exact gate in an isolated Linux container.
when_to_use: Use when fresh Go test or generator binaries produce no test output, time out across unrelated packages, and process samples remain in _dyld_start while macOS security services are saturated.
---

# macOS dyld Go validation fallback

Confirm that the failure precedes application startup before changing code:

- Compile a focused test binary with `go test -c`.
- Run the binary directly with a short test timeout.
- Sample the stalled process into the Detent-provided temporary directory.
- Treat a sample dominated by `_dyld_start`, together with a saturated macOS security service, as a host execution failure rather than a Go test failure.
- Verify the binary's code signature before choosing a fallback.

Run the repository's exact validation target in a Linux container when Docker is healthy. Mount the worktree, the worktree's shared Git metadata at its original absolute path, and Detent-scoped module, build, and tool caches. Pass `safe.directory` through `GIT_CONFIG_COUNT`, `GIT_CONFIG_KEY_0`, and `GIT_CONFIG_VALUE_0` so generators can resolve repository-relative paths without changing global Git configuration.

Run the container as the workspace owner. Use workspace-owned temporary directories for `node_modules` and npm cache; anonymous Docker volumes default to root ownership. Prefer repository-pinned prebuilt tools when host file-table pressure makes large `go install` dependency trees fail.

Re-run generation with Git repository detection enabled and confirm that unrelated generated files remain unchanged. Record both the native startup evidence and the successful exact containerized gate in the handoff.
