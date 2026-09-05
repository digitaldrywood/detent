---
name: wildcard-loopback-test-isolation
description: Diagnose HTTP test EOFs or wrong responses when a wildcard listener is probed through loopback on macOS.
when_to_use: Use when parallel HTTP tests intermittently reach the wrong handler or lose a connection while the intended wildcard server remains alive.
---

# Verify ownership of the probe address

- Compare the listener address with the client's actual destination. A listener on `0.0.0.0:0` does not necessarily reserve the assigned port on `127.0.0.1`. macOS can allow both listeners simultaneously; verify the host behavior with `net.Listen` before assigning causality.
- Hold both listeners on the same explicit ephemeral port. Give their handlers distinguishable responses, then probe loopback to establish which listener receives the connection. Keep every socket outside production ports.
- Reproduce a close-before-response EOF with the shadowing loopback handler and `panic(http.ErrAbortHandler)`. To exercise `httptest.Server.Close` itself, wrap the loopback listener's `Accept`: read the request with `http.ReadRequest`, signal receipt, and wait on a channel released by the wrapper's `Close`. Close the shadow server after receipt. Reading the request before closing avoids an unread-data TCP reset obscuring the EOF sequence.
- Keep the evidence boundary explicit: reproducing a possible interleaving establishes a failure mechanism, not proof that an untraced historical failure used that interleaving. Shared transport cleanup and ambient authentication are separate hypotheses.
- For tests of wildcard-to-loopback URL mapping, bind the HTTP fixture to the exact loopback destination. Inject the occupied bind result through the existing listener dependency, asserting that production requested the configured wildcard address. This separates platform bind rules from HTTP health classification.
- Assert that binding another listener to the fixture's probe address fails as occupied. Demonstrate this assertion failing against the original wildcard fixture, then passing with the loopback fixture. Keep real connection-close and incomplete-response cases classified as failures; do not add generic EOF retries.
- When supplying an existing listener to `httptest.Server`, construct the server with that listener and its `http.Server` configuration. Replacing `NewUnstartedServer().Listener` without closing the original discards a live socket.
