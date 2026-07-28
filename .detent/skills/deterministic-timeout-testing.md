---
name: deterministic-timeout-testing
description: Test timeout renewal, expiration, and duration limits with explicit control instead of wall-clock margins.
when_to_use: Use when a test sleeps for a timeout, expects a timer to fire within a margin, or asserts elapsed time under hosted-runner load.
---

# Test timeout behavior with controllable time

- Confirm the failure is an elapsed-time assumption by reading the hosted-runner log and identifying the sleep, timer, deadline, or elapsed assertion.
- Inject a private timer or context factory while preserving the real production default.
- Give the test double explicit reset acknowledgements and an expiration operation. Synchronize the work under test with channels so expiration happens only after the intended stage is reached.
- Test progress propagation separately when the timer loop consumes the same progress channel; do not make the assertion compete with production for one signal.
- Preserve deadline metadata when callers inspect `Context.Deadline`, but trigger cancellation explicitly with the configured cause.
- Keep real time only as a generous deadlock guard around explicit synchronization or OS integration, and document that it is not a behavioral margin.
- Run focused cases repeatedly under `-race`, then run the affected packages with `-race -count=10` and the repository validation gate.
