---
name: same-invocation-race-coverage
description: Reduce duplicate fixture work by collecting coverage during a required fresh race run.
when_to_use: Use when a serialized Go gate repeats expensive test fixtures for separate race and coverage checks.
---

- Measure the complete package with race, coverage, and combined atomic coverage. Retain command wall time, test time, CPU, memory, selected inputs, and cache state. Separate lock queueing and remote CI from execution cost.
- Check race build tags and assertions first. Non-race tests may enforce performance deadlines that race builds disable. Limit reuse to packages with equivalent source selection; retain ordinary execution elsewhere. Fail closed on a selection mismatch.
- Reuse only coverage produced by the successful current invocation. Preserve required race flags, fresh execution, package timeout, and concurrency limits. Do not turn timing telemetry or a command hash into a gate receipt.
- Generate profiles in unique temporary directories. Reject missing, empty, malformed, incompatible, or overlapping profiles before publishing. Rename the final output within its destination filesystem only after every test group succeeds.
- Preserve ordinary coverage instrumentation elsewhere. If coverage consumers only use hit/miss status, normalize atomic and set profiles to a set profile instead of adding atomic instrumentation to unrelated packages. Validate each input mode and count before normalization; preserve statement weights and uncovered blocks.
- Exercise failed groups, changed selections, stale published files, and interrupted-run leftovers with deterministic subprocess fixtures. Verify a real race still fails with coverage enabled.
- Verify progress from actual tool-output events through published usage telemetry; seeded prose snapshots only prove rendering. Keep partial output bounded per tool, distinguish output from command payloads, and preserve the final assistant response.
- Keep aggregate, package, and exact-file coverage floors and generated exclusions. Preserve the separate ordinary CI checks. Expand independent capacity only after recorded resource and isolation evidence supports it.
