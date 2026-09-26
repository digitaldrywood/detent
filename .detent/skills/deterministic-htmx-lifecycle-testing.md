---
name: deterministic-htmx-lifecycle-testing
aliases:
  - native-sse-cleanup-verification
description: "Verify HTMX lifecycle tests and SSE cleanup without racing DOM swaps or leaving idle streams."
when_to_use: "Use when a focused HTMX test passes but a serial browser suite exposes duplicate nodes, settling classes, or assertions that race a synthetic lifecycle event. Also use for native sse cleanup verification."
---

# Test HTMX lifecycle behavior deterministically

- Reproduce the failure in the full serial file and inspect the transient DOM before changing waits.
- Trace both application listeners and HTMX's default handling for the dispatched lifecycle event. A synthetic event can exercise framework mutation as well as application behavior.
- Remove impossible target and payload combinations. Test only event shapes the server and HTMX extension can produce in normal operation.
- Preserve the behavior contract with realistic same-state and changed-state cases instead of asserting before an unintended swap wins a race.
- Wait on observable completion conditions such as the intended element, settled DOM, or application state. Do not use sleeps or assertions that depend on catching a transient frame.
- Run the focused case, the complete serial file with retries disabled, and the repository's full browser gate.

## Native SSE cleanup verification

Use this case when removing or replacing an HTMX subtree should close its EventSources, especially when streams can remain idle.

- Before navigation, subclass the browser's native EventSource and retain each constructed instance. Preserve native connection and readyState behavior; do not substitute a fake stream.
- Use an isolated server with frozen demo data or otherwise idle streams. Wait for each owned source to become OPEN before triggering removal.
- Close the real sheet or switch tabs, then assert the removed sources are CLOSED immediately after DOM removal. Never inject a later message or error: that can trigger missing-node cleanup and hide the defect.
- Record `htmx:sseClose` on each source owner before it is detached. Assert the cleanup reason is `nodeReplaced`, not `nodeMissing`, for the SSE extension version in use.
- Repeat open/close and attach/detach cycles. Assert the board retains the same native source, stays OPEN, and preserves its connection state and enabled controls.
- When fixing teardown, inspect the loaded HTMX implementation. A DOM removal helper may omit recursive `htmx:beforeCleanupElement`; use a supported swap lifecycle that runs cleanup instead of manually reaching into extension internals.
- Run at desktop and mobile viewport sizes and keep native ready-state assertions in the browser regression suite.
