---
name: isolated-go-ui-snapshot-preview
description: Browser-verify a Go-rendered Detent UI state without modifying the live dogfood process or committing preview fixtures.
when_to_use: Use when a UI condition needs a seeded telemetry snapshot that the running instance cannot safely reproduce.
---

# Isolated Go UI snapshot preview

1. Put a temporary package test and Go overlay JSON under the Detent-provided temporary directory. Map a virtual `_test.go` path inside the target package to the temporary test file so the worktree stays unchanged.
2. In the test, construct the real server dependencies, publish the required telemetry snapshot, and serve the real handler with `httptest.NewServer`. Print the random-port URL and wait for a temporary completion marker with a bounded deadline.
3. Run only the overlay test in a managed terminal session. Open the printed URL with the worktree-aware Chrome DevTools MCP; never bind to or mutate the live port-4000 process.
4. Inspect a semantic page snapshot before taking a viewport screenshot. Exercise expandable alerts and related health pages so both terse and detailed copy are verified.
5. Close the Chrome page before creating the completion marker. Open SSE connections otherwise keep `httptest.Server.Close` blocked.
6. Wait on the original test session and require a passing exit. Let Detent remove the temporary overlay, test, marker, and browser context after the worker exits.
