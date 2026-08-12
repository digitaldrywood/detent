---
name: ambient-auth-test-isolation
description: Diagnose HTTP test timeouts, EOF responses, and unexpected authorization statuses caused by inherited production credentials.
when_to_use: Use when local HTTP tests fail only inside an authenticated agent or CI environment, especially when a healthy listener appears unready or handlers receive an unexpected bearer token.
---

# Isolate HTTP tests from ambient authentication

- Inspect response status and body before treating a readiness deadline as a listener failure. Record credential presence or length without printing secret values.
- Trace client and server credential precedence, including environment overrides, configured test tokens, and default lookup functions.
- Rerun the unchanged failing command with only the suspected credential variable blanked. Treat a pass as evidence of environment contamination, not proof that production precedence is wrong.
- Remember that `t.Fatalf` in an HTTP handler exits that handler goroutine before it writes a response, which can surface to the client as EOF.
- Preserve production environment defaults. Inject an empty lookup for a targeted test, or clear the ambient credential once in package `TestMain` when the package broadly assumes unauthenticated defaults.
- Do not mutate environment variables from parallel tests. Keep a separate explicit test proving that the production environment override still wins when configured.
- Validate with the original ambient credential present: repeat the focused test under `-race`, run the explicit authentication contract tests, repeat affected packages under `-race`, and run the repository gate.
