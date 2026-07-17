---
name: deterministic-debounce-testing
description: Stabilize debounce coalescing tests by controlling event delivery and timer expiry without weakening assertions.
when_to_use: Use when scheduler pressure lets a real debounce timer expire between back-to-back test actions, especially under race or concurrent suites.
---

# Test debounce coalescing deterministically

- Confirm the intermediate value matches the observed failure before treating the problem as test timing rather than production behavior.
- Inject private event-source and timer factories while keeping production defaults unchanged. Preserve real event-source integration coverage in separate tests.
- Give the controlled timer observable reset acknowledgements and an explicit fire operation. Use bounded real-time deadlines only to detect a stuck test, never to arrange the debounce cycle.
- Write the first value, deliver its matching event, and wait for the first timer reset. Write the second value, deliver its event, and wait for the second reset. Fire the timer exactly once.
- Assert the first delivered update contains the second value and that no additional update follows. Never drain and ignore intermediate updates because that stops testing coalescing.
- Run the focused test repeatedly with the race detector, then run the repository's full validation gate.
