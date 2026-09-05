---
name: native-sse-cleanup-verification
description: Verify prompt HTMX SSE teardown without masking detached idle-source leaks.
when_to_use: Use when removing or replacing an HTMX subtree should close its EventSources, especially when streams can remain idle.
---

# Native SSE cleanup verification

- Before navigation, subclass the browser's native EventSource and retain each constructed instance. Preserve native connection and readyState behavior; do not substitute a fake stream.
- Use an isolated server with frozen demo data or otherwise idle streams. Wait for each owned source to become OPEN before triggering removal.
- Close the real sheet or switch tabs, then assert the removed sources are CLOSED immediately after DOM removal. Never inject a later message or error: that can trigger missing-node cleanup and hide the defect.
- Record `htmx:sseClose` on each source owner before it is detached. Assert the cleanup reason is `nodeReplaced`, not `nodeMissing`, for the SSE extension version in use.
- Repeat open/close and attach/detach cycles. Assert the board retains the same native source, stays OPEN, and preserves its connection state and enabled controls.
- When fixing teardown, inspect the loaded HTMX implementation. A DOM removal helper may omit recursive `htmx:beforeCleanupElement`; use a supported swap lifecycle that runs cleanup instead of manually reaching into extension internals.
- Run at desktop and mobile viewport sizes and keep native ready-state assertions in the browser regression suite.
