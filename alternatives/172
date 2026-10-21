# Mobile kanban long-press investigation

Baseline: `71c37f6e` (macOS, local Playwright Chromium, one worker).
The issue's original failure was not reproduced. No application code, gesture,
polling interval, timeout, or synchronization was changed.

## Recorded experiments

- Full suite with passive event/scroll capture: **183 passed, 32 skipped (7.3m)**.
  Command: `node_modules/.bin/playwright test`.
- Same instrumented long-press test, 20 repetitions: **20 passed**.
  Command: `node_modules/.bin/playwright test --project mobile-chromium --grep
  'project kanban supports long-press' --repeat-each 20`.
- Diagnostic failure check: temporarily forced the existing target-lane predicate
  to return false, ran the focused test, then restored it. The expected failure
  retained an attachment with **350 events and 13 lane samples**, not truncated.
  This verifies diagnostics; it does **not** reproduce the reported defect.

`full-suite-gesture.json` contains selected events from the passing full-suite
run, with times relative to the edge touch move. Todo moved from x=390 to x=20;
release occurred while the drag remained active and the dashboard connected.
The run used the same gesture and assertions as the baseline. Instrumentation
itself can affect timing, so these passes do not establish absence of a race.

Final helper verification: **3 focused runs passed (45s)**. `make check-fast`
passed, including invariant checks, build, lint, vet, and Go tests. Independent
validator review: approve, score 0.96, no P1/P2 findings.

## What remains unknown

`advanceTouchLane` advances every 500 ms while the touch stays at the edge.
Playwright's default `expect.poll` delays are 100, 250, 500, then 1000 ms.
A lane could pass between samples, but no naturally failing event sequence was
recorded in this investigation. Neither load, inertia, cancellation, snapshot
morphing, nor polling is established as the original cause. Do not change
synchronization based on the interval comparison alone.

## Evidence for the next occurrence

The long-press test now attaches `kanban-touch-evidence` to its Playwright report
on success and failure. It records up to 2000 events, reports truncation, and
includes the actual bounding-box poll samples. Event capture uses passive
listeners and does not dispatch events or change timers. Runtime and CDP cleanup
still runs if attachment creation fails.

Inspect the attachment alongside the retained failure trace/video. Compare the
last edge touch move, touch identifiers and cancellation, connection changes,
scroll/scrollend geometry, snapshot settle events, and lane samples. Determine
whether Todo never arrives, passes between samples, or the drag ends first.
The pre-existing pending request error can accompany the lane assertion failure;
it is not evidence of a rejected move, since the drop has not happened yet.

All runtimes used ephemeral ports and isolated temporary homes. The live instance
on port 4000 was untouched. Hosted form readiness (#2585) remains separate.
