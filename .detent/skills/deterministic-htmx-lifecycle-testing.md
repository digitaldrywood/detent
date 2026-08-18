---
name: deterministic-htmx-lifecycle-testing
description: Stabilize HTMX browser tests that dispatch synthetic lifecycle events without racing the framework's own DOM swaps.
when_to_use: Use when a focused HTMX test passes but a serial browser suite exposes duplicate nodes, settling classes, or assertions that race a synthetic lifecycle event.
---

# Test HTMX lifecycle behavior deterministically

- Reproduce the failure in the full serial file and inspect the transient DOM before changing waits.
- Trace both application listeners and HTMX's default handling for the dispatched lifecycle event. A synthetic event can exercise framework mutation as well as application behavior.
- Remove impossible target and payload combinations. Test only event shapes the server and HTMX extension can produce in normal operation.
- Preserve the behavior contract with realistic same-state and changed-state cases instead of asserting before an unintended swap wins a race.
- Wait on observable completion conditions such as the intended element, settled DOM, or application state. Do not use sleeps or assertions that depend on catching a transient frame.
- Run the focused case, the complete serial file with retries disabled, and the repository's full browser and validation gates.
