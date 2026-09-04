---
name: deterministic-http-transport-cleanup-testing
description: "Reproduce HTTP client failures caused by concurrent test-server cleanup closing a shared process-wide transport."
when_to_use: "Use when parallel Go HTTP tests intermittently fail with transport lifecycle errors such as `http: CloseIdleConnections called` while another `httptest.Server` is closing."
---

# Test shared HTTP transport cleanup deterministically

- Verify the active Go toolchain's `httptest.Server.Close` implementation before assigning causality. It may call `http.DefaultTransport.CloseIdleConnections`, so separate `http.Client` values with nil transports still share lifecycle state.
- Replace `http.DefaultTransport` in one non-parallel top-level test with a close-sensitive `http.RoundTripper`. Have `RoundTrip` signal that delivery started, wait for `CloseIdleConnections`, and then return the recorded transport error. Restore the original transport with test cleanup before parallel tests resume.
- Use a separate `httptest.Server` solely to invoke the real cleanup path. Coordinate delivery and cleanup with channels and `sync.Once`; do not use sleeps to widen the race.
- Arrange the readiness signal so both implementations progress: the close-sensitive transport signals it when the buggy client uses the process default, while the target server handler signals it when an isolated client reaches the server. This lets the unchanged test fail with the observed error before the fix and complete without timing assumptions afterward.
- Assert the ownership invariant separately by constructing two clients and verifying both transports are non-nil and distinct.
- Isolate lifecycle state by cloning the standard `*http.Transport` for each client. If the process default can be a custom `RoundTripper`, use an independent standard-settings fallback rather than silently sharing the custom transport.
- Prove the regression red against the old constructor, then repeat the focused test, the affected package with a high `-count`, the focused package under `-race`, and the repository validation gate.
