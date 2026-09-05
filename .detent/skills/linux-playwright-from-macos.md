---
name: linux-playwright-from-macos
description: Validate Linux-only Playwright failures for a Go web application from a macOS worktree using a matching browser container and cross-compiled server.
when_to_use: Use when browser tests pass on macOS but Linux CI fails on layout, focus, scrolling, or lifecycle behavior.
---

# Linux Playwright validation from macOS

Read the failed CI job's assertions and trace before changing waits or baselines. A passing macOS repetition does not establish that a Linux browser failure is fixed. If Docker becomes unavailable, record the incomplete Linux validation explicitly.

Use the installed Playwright package version to select the matching `mcr.microsoft.com/playwright` image. Check Docker availability, existing images, and the image architecture first. Cross-compile the Go server with `GOOS=linux` and the matching `GOARCH` into a separate worktree output such as `tmp/detent-linux-arm64`; preserve the native binary used by local gates.

Mount the worktree into the browser container, set its working directory to that mount, and point `DETENT_BINARY` at the Linux binary. Mount the Detent-provided temporary directory separately and set the container's `TMPDIR` to that mount so isolated runtime homes remain scoped to the attempt. Use the existing ephemeral-port runtime helper. The browser image supplies Chromium; the checkout supplies its matching Playwright test runner. This avoids installing a second Node dependency tree or changing lockfiles.

Run the failing case first, followed by the complete serial file or full browser suite with retries disabled and strict screenshot comparison enabled. Keep existing geometry and visibility assertions. Update baselines only for reviewed product changes.

For tooltip failures around viewport changes, inspect focus and scroll ownership. A scroll handler may intentionally close a tooltip while leaving its trigger focused; calling `focus()` again is then a no-op. Model a fresh focus gesture after scrolling settles instead of repeatedly polling a reveal that no event will reopen. Confirm this event sequence independently before changing the test.

Record the container image, architecture, focused result, and full-suite result in the Workpad. Run the repository's normal validation gate as well; the browser container validates Linux browser behavior, not the entire Go/tooling contract.
