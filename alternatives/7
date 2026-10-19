---
name: cumulative-race-suite-budget
aliases:
  - same-invocation-race-coverage
description: "Diagnose race-suite timeouts and combine required race and coverage evidence without repeating expensive fixtures."
when_to_use: "Use when an unchanged Go package alternates between passing race tests and hitting its package timeout while many subtests wait for parallel slots. Also use for same invocation race coverage."
---

Capture the failing revision, toolchain, runner constraints, complete test
selection, package duration, and timeout goroutine dump before changing a
deadline. Active-test ages describe those tests, not elapsed package work.

Run uncached complete-package samples on constrained Linux with synthetic
fixtures. Retain `go test -json` events and compare run/pause/cont/terminal
timestamps. Report queue time separately from Go's elapsed field; parents,
children, and parallel fixtures overlap and cannot be summed as wall time.

Time shared fixture constructors and take a CPU profile. State which direct
fixture paths are excluded. Correlate recent completions and active stacks:
runnable migrations plus changing tests support cumulative work; one
unchanging active operation needs a separate lifecycle investigation.

Prefer a narrowly scoped package budget and bounded fixture concurrency
when preserving migration/fixture coverage matters. Keep every test,
assertion, workload size, race check, and individual lifecycle deadline.
Use the same command in local validation and CI, and upload raw evidence
on failures so future timeouts remain diagnosable.

Repeat representative samples. If the recorded failure includes tests on an
unmerged branch, validate its exact head in a disposable checkout without
importing its feature changes. Record architecture limits and justify
headroom from observed package work, not the single slowest test.

## Same Invocation Race Coverage

Use this case when a serialized Go gate repeats expensive test fixtures for separate race and coverage checks.

- Measure the complete package with race, coverage, and combined atomic coverage. Retain command wall time, test time, CPU, memory, selected inputs, and cache state. Separate lock queueing and remote CI from execution cost.
- Check race build tags and assertions first. Non-race tests may enforce performance deadlines that race builds disable. Limit reuse to packages with equivalent source selection; retain ordinary execution elsewhere. Fail closed on a selection mismatch.
- Reuse only coverage produced by the successful current invocation. Preserve required race flags, fresh execution, package timeout, and concurrency limits. Do not turn timing telemetry or a command hash into a gate receipt.
- Generate profiles in unique temporary directories. Reject missing, empty, malformed, incompatible, or overlapping profiles before publishing. Rename the final output within its destination filesystem only after every test group succeeds.
- Preserve ordinary coverage instrumentation elsewhere. If coverage consumers only use hit/miss status, normalize atomic and set profiles to a set profile instead of adding atomic instrumentation to unrelated packages. Validate each input mode and count before normalization; preserve statement weights and uncovered blocks.
- Exercise failed groups, changed selections, stale published files, and interrupted-run leftovers with deterministic subprocess fixtures. Verify a real race still fails with coverage enabled.
- Verify progress from actual tool-output events through published usage telemetry; seeded prose snapshots only prove rendering. Keep partial output bounded per tool, distinguish output from command payloads, and preserve the final assistant response.
- Keep aggregate, package, and exact-file coverage floors and generated exclusions. Preserve the separate ordinary CI checks. Expand independent capacity only after recorded resource and isolation evidence supports it.
