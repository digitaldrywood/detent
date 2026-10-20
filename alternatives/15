---
name: diagnose-dispatch-loops
description: Diagnose and stop Detent issues that repeatedly dispatch in one lane despite successful or mixed terminal outcomes.
when_to_use: Use when one issue runs repeatedly without lane, diff, commit, or pull-request advancement, especially when attempt numbers reset or other circuit breakers do not fire.
---

# Diagnose dispatch loops

1. Reproduce the recorded terminal-attempt sequence before testing a suspected narrative cause.
2. Trace what `attempt_number` represents. Treat a reset retry series separately from durable same-issue, same-lane dispatch history.
3. Build the progress fingerprint from the lane, workspace diff, workspace head, and pull-request identity and movement. Do not treat Workpad or audit-only changes as product progress.
4. Count consecutive durable terminal completions regardless of success or failure. Reset only on a changed progress fingerprint or lane advancement, and fail open when required evidence is unavailable.
5. Require at least two dispatches so one legitimately slow run cannot trip the loop brake.
6. Park with a distinct loop cause and make that cause sticky against automatic recovery paths that would immediately recreate the loop. Preserve explicit recovery when new deliverable evidence appears.
7. Surface the repeated issue in health before the trip threshold, including the issue, lane, count, limit, and latest completion time.
8. Test successful and failed repetition, each reset signal, single-run behavior, the distinct park cause, default activation without spend configuration, and recovery interactions.
