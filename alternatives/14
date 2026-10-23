---
name: deterministic-timeout-testing
aliases:
  - deterministic-debounce-testing
description: "Test timeout and debounce behavior with controlled timers and explicit event delivery."
when_to_use: "Use when a test sleeps for a timeout, expects a timer to fire within a margin, or asserts elapsed time under hosted-runner load. Also use for deterministic debounce testing."
---

# Test timeout behavior with controllable time

- Confirm the failure is an elapsed-time assumption by reading the hosted-runner log and identifying the sleep, timer, deadline, or elapsed assertion.
- Inject a private timer or context factory while preserving the real production default.
- Give the test double explicit reset acknowledgements and an expiration operation. Synchronize the work under test with channels so expiration happens only after the intended stage is reached.
- Test progress propagation separately when the timer loop consumes the same progress channel; do not make the assertion compete with production for one signal.
- Preserve deadline metadata when callers inspect `Context.Deadline`, but trigger cancellation explicitly with the configured cause.
- Keep real time only as a generous deadlock guard around explicit synchronization or OS integration, and document that it is not a behavioral margin.
- Run focused cases repeatedly under `-race`, then run the affected packages with `-race`.

## Test debounce coalescing deterministically

Use this case when scheduler pressure lets a real debounce timer expire between back-to-back test actions, especially under race or concurrent suites.

- Confirm the intermediate value matches the observed failure before treating the problem as test timing rather than production behavior.
- Inject private event-source and timer factories while keeping production defaults unchanged. Preserve real event-source integration coverage in separate tests.
- Give the controlled timer observable reset acknowledgements and an explicit fire operation. Use bounded real-time deadlines only to detect a stuck test, never to arrange the debounce cycle.
- Write the first value, deliver its matching event, and wait for the first timer reset. Write the second value, deliver its event, and wait for the second reset. Fire the timer exactly once.
- Assert the first delivered update contains the second value and that no additional update follows. Never drain and ignore intermediate updates because that stops testing coalescing.
- Run the focused test repeatedly with the race detector.
