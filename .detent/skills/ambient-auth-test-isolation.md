---
name: ambient-auth-test-isolation
description: "Isolate HTTP and filesystem test fixtures from ambient credentials, cleanup, shared transports, registries, and loopback conflicts."
when_to_use: "Use when local HTTP tests fail only inside an authenticated agent or CI environment, especially when a healthy listener appears unready or handlers receive an unexpected bearer token. Also use for wildcard loopback test isolation, shared registry fixture isolation, deterministic http transport cleanup testing, ambient cleanup test isolation."
---

# Isolate HTTP tests from ambient authentication

- Inspect response status and body before treating a readiness deadline as a listener failure. Record credential presence or length without printing secret values.
- Trace client and server credential precedence, including environment overrides, configured test tokens, and default lookup functions.
- Rerun the unchanged failing command with only the suspected credential variable blanked. Treat a pass as evidence of environment contamination, not proof that production precedence is wrong.
- Remember that `t.Fatalf` in an HTTP handler exits that handler goroutine before it writes a response, which can surface to the client as EOF.
- Preserve production environment defaults. Inject an empty lookup for a targeted test, or clear the ambient credential once in package `TestMain` when the package broadly assumes unauthenticated defaults.
- Do not mutate environment variables from parallel tests. Keep a separate explicit test proving that the production environment override still wins when configured.
- Validate with the original ambient credential present: repeat the focused test under `-race`, run the explicit authentication contract tests, repeat affected packages under `-race`.

## Verify ownership of the probe address

Use this case when parallel HTTP tests intermittently reach the wrong handler or lose a connection while the intended wildcard server remains alive.

- Compare the listener address with the client's actual destination. A listener on `0.0.0.0:0` does not necessarily reserve the assigned port on `127.0.0.1`. macOS can allow both listeners simultaneously; verify the host behavior with `net.Listen` before assigning causality.
- Hold both listeners on the same explicit ephemeral port. Give their handlers distinguishable responses, then probe loopback to establish which listener receives the connection. Keep every socket outside production ports.
- Reproduce a close-before-response EOF with the shadowing loopback handler and `panic(http.ErrAbortHandler)`. To exercise `httptest.Server.Close` itself, wrap the loopback listener's `Accept`: read the request with `http.ReadRequest`, signal receipt, and wait on a channel released by the wrapper's `Close`. Close the shadow server after receipt. Reading the request before closing avoids an unread-data TCP reset obscuring the EOF sequence.
- Keep the evidence boundary explicit: reproducing a possible interleaving establishes a failure mechanism, not proof that an untraced historical failure used that interleaving. Shared transport cleanup and ambient authentication are separate hypotheses.
- For tests of wildcard-to-loopback URL mapping, bind the HTTP fixture to the exact loopback destination. Inject the occupied bind result through the existing listener dependency, asserting that production requested the configured wildcard address. This separates platform bind rules from HTTP health classification.
- Assert that binding another listener to the fixture's probe address fails as occupied. Demonstrate this assertion failing against the original wildcard fixture, then passing with the loopback fixture. Keep real connection-close and incomplete-response cases classified as failures; do not add generic EOF retries.
- When supplying an existing listener to `httptest.Server`, construct the server with that listener and its `http.Server` configuration. Replacing `NewUnstartedServer().Listener` without closing the original discards a live socket.

## Isolate registry lifetime from fixture lifetime

Use this case when Go tests pass alone but repeated runs skip fake HTTP handlers or inherit synthetic rate-limit responses.

- Reproduce with the smallest focused `go test -count=2` command before changing fixtures. Count repetitions share a process and package globals.
- Trace synthetic responses back to their registry and exact key. A new client or closed HTTP server does not imply a new registry; an ephemeral port can be reused while an old entry remains active.
- Recreate the failure deterministically with fake HTTP clients using the same endpoint and credential. Seed a backoff through the real request path, then construct a new client with a healthy transport and assert whether that transport is reached.
- Cover shared registry, private registry, different endpoint, and different credential in a table. Avoid waiting for the OS to reuse a port or for real backoff timers to expire.
- Isolate fixtures that produce persistent state using an existing instance registry seam. Do not reset package globals while parallel tests can use them, disable production brakes, or retry failing assertions.
- Preserve a test of default cross-client sharing with a unique per-invocation credential; a test name alone repeats under `-count`.
- Run repeated race tests for the whole affected package.

## Test shared HTTP transport cleanup deterministically

Use this case when parallel Go HTTP tests intermittently fail with transport lifecycle errors such as `http: CloseIdleConnections called` while another `httptest.Server` is closing.

- Verify the active Go toolchain's `httptest.Server.Close` implementation before assigning causality. It may call `http.DefaultTransport.CloseIdleConnections`, so separate `http.Client` values with nil transports still share lifecycle state.
- Replace `http.DefaultTransport` in one non-parallel top-level test with a close-sensitive `http.RoundTripper`. Have `RoundTrip` signal that delivery started, wait for `CloseIdleConnections`, and then return the recorded transport error. Restore the original transport with test cleanup before parallel tests resume.
- Use a separate `httptest.Server` solely to invoke the real cleanup path. Coordinate delivery and cleanup with channels and `sync.Once`; do not use sleeps to widen the race.
- Arrange the readiness signal so both implementations progress: the close-sensitive transport signals it when the buggy client uses the process default, while the target server handler signals it when an isolated client reaches the server. This lets the unchanged test fail with the observed error before the fix and complete without timing assumptions afterward.
- Assert the ownership invariant separately by constructing two clients and verifying both transports are non-nil and distinct.
- Isolate lifecycle state by cloning the standard `*http.Transport` for each client. If the process default can be a custom `RoundTripper`, use an independent standard-settings fallback rather than silently sharing the custom transport.
- Prove the regression red against the old constructor, then repeat the focused test, the affected package with a high `-count`, the focused package under `-race`.

## Confirm the real bodyless-response window

When a valid 204 is reported as a transport failure, prefer a real HTTP/1 transport reproduction before using a synthetic close-sensitive transport:

- Inspect the pinned Go version's bodyless-response read loop. It can pool the connection before delivering the response to RoundTrip.
- Attach httptrace.ClientTrace.PutIdleConn to the request. Signal entry through a channel and hold the callback on another channel; this parks the real response between pooling and delivery.
- After the signal, close an unrelated httptest.Server. With the shared default transport, wait for RoundTrip to return before releasing the callback. Assert the bounded error category identifies CloseIdleConnections, not a context deadline. Join the callback before ending the test.
- Run a control using the fixture-owned server.Client transport: close the unrelated server at the same point, release the callback, and assert successful response delivery. Check that fixture transports are non-nil, distinct from the default, and distinct across servers.
- Keep the global-default control non-parallel. Bound stalled requests with context cancellation and release trace callbacks on failure. Do not use sleeps or manufacture the expected transport error.
- For a fixture-only defect, inject server.Client with the intended timeout and redirect policy. Do not change production authorization semantics or add retries without separate production evidence.
- Distinguish a demonstrated failure mechanism from attribution of an earlier uninstrumented incident. Report named operations, response status, error type, and allowlisted failure categories; omit raw URLs, headers, bodies, and arbitrary transport error text.

## Ambient Cleanup Test Isolation

Use this case when: A new cleanup hook uses a test runtime database but resolves artifact paths from the real process environment, or a filesystem diagnostic consumes an otherwise mocked test's deadline.

Before exercising a new deletion hook through startup or a reaper, inspect the
whole integration-test wiring, including `TestMain`. A temporary runtime store
alone is insufficient: it has no active references to the developer's real
provider artifacts and can therefore incorrectly authorize their removal.

Give the test process a disposable provider home through the same environment
variable production resolves (for example, `CODEX_HOME`). Create it under the
provided temporary directory, preserve tests' explicit fixture overrides, and
remove it before `os.Exit`; deferred cleanup does not run after `os.Exit`.
Inspect explicit provider-home assignments in test commands as well.

For read-only diagnostics, use the existing dependency-injection boundary so a
mocked doctor run does not scan the host's real artifact tree. Test the real
scanner separately against bounded fixture trees. If a previously stable
short-deadline test fails after adding a diagnostic, reproduce it alone and
check ambient filesystem work before increasing its timeout.

Validate the cleanup itself with fixtures for ownership, age boundaries, active
references, and shared files. Then exercise the lifecycle hook with the isolated
provider home. A successful test does not establish that earlier unisolated
runs left production artifacts unchanged; avoid claiming that without evidence.
