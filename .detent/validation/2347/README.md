# Serialized validation overhead

## Recorded queue and execution cost

September 8 operator observations distinguish three independent costs:

| Recorded run | Local queue | Local gate | Remote CI |
| --- | ---: | ---: | ---: |
| [#2328 final retry](https://github.com/digitaldrywood/detent/issues/2328#issuecomment-5588864734) | about 19m | about 16m | 20m38s; Windows portability 20m33s |
| [#2345 retry](https://github.com/digitaldrywood/detent/issues/2345#issuecomment-5590883393) | 15m deadline, then about 19m | passed | separate PR checks |
| [#2340 retry](https://github.com/digitaldrywood/detent/issues/2340#issuecomment-5590809498) | 15m deadline, then about 28m | running when observed | not yet started |

The unchanged FIFO diagnostics identify the owner PID, ownership start,
queue position, queue length and elapsed wait. `make check` now also appends
`tmp/validation-events.jsonl`: schema, process PID, invocation start,
event time, phase, wait seconds, run seconds and command process CPU time.
Join PID and invocation start to distinguish restarts and PID reuse.
CPU accounting is whatever the platform reports for the command process;
it is not a fleet-wide CPU measurement. The command SHA-256 groups command
arguments for diagnosis, **never** authorizes result reuse. A waiting event
without a terminal event can represent process death, not a passing gate.

## Repeated work and cache conditions

[Earlier complete Hub profiling](../2307/README.md) attributed 48.57% of
sampled CPU to SQLite migration stacks. Separate race and coverage runs
repeat these fixtures. [Cache diagnostics](cache-diagnostics.txt) show the
same activity test executable IDs with different fixture input IDs and
missing cached output. The input log includes temporary fixture paths.
Hub race execution explicitly uses `-count=1`, so its successful test result
must not be cached across gates.

The operator enabled the existing `workspace.cache_strategy: shared` on
September 8 at 20:46Z. The initial retry still received isolated `.detent/tmp`
Go build and module caches and downloaded Go 1.26.6 plus dependencies.
After an operator-confirmed restart during measurement, the resumed worker
received shared caches under `workspaces/.detent/cache/detent-6383f7eb0b6a`.
No cache environment variable was repointed. Both comparisons warm build
variants before measuring test execution, so they cannot establish a cold
startup speedup from shared caches. Persistent build/module cache reuse is
a separate operator configuration improvement; it does not make an entire
gate result reusable.

The [shared-cache control](shared-cache-diagnostics.txt) runs unchanged
`TestValidationPhase` twice with identical flags and Go 1.26.6. The first
execution took 0.373s; the second reports `(cached)`. This confirms ordinary
Go result reuse for stable inputs, unlike the fixture-driven misses above.
It is not a measurement of total gate or cross-turn startup improvement.

## Bounded capacity decision

[Resource samples](resources.jsonl) record 2,220–2,249 processes, 20–101
runnable processes, and one-minute load averages from about 23 to 164 on
the 16-logical-CPU, 128-GiB host. The selected Go/test process RSS reached
about 4.6 GiB; that is not total host memory or peak gate memory. These
samples do not demonstrate safe capacity for a second full gate. Keep one
slot, existing FIFO admission, cancellation cleanup, process-death recovery
and the existing distinction between queue deadlines and active execution.

## Same-invocation coverage

The default local combined target collects Hub atomic coverage during the
same required fresh race invocation. It retains `-race -count=1 -p=1`,
parallelism 2, the 15-minute package timeout and full test selection.
Ordinary and race Hub source selections must match before collection.
Other packages retain separate race and ordinary coverage executions:
non-race web builds enforce performance assertions that race builds disable.
Their ordinary coverage also retains its existing `set` instrumentation.
The merger accepts only disjoint fresh `set`/`atomic` profiles and normalizes
positive execution counts to one in a `set` output. Aggregate, package and
exact-file floors use hit/miss status, so this preserves every coverage
decision without adding atomic instrumentation to unrelated packages.

This is not a persistent gate cache. Code, generated sources, dependency and
toolchain selection, environment, and test flags are those of the actual
current Go invocation. No prior commit, prior attempt, remote check, or
command hash supplies a passing result. Build, generated-input checks, lint,
vet, NilAway, race checks, aggregate/package/exact-file coverage floors and
generated-code exclusions remain required. Remote branch checks are unchanged.

Each invocation creates unique temporary profiles, checks every test group
exit, rejects absent/empty/malformed/incompatible/overlapping profiles, and
publishes via a rename in the output directory only after all groups pass.
A failed gate leaves prior published output untouched but returns failure;
that output is never read to satisfy the new combined gate. Restarted gates
ignore interrupted temporary directories and rerun tests. No inter-invocation
equivalence judgment or durable passing receipt is introduced.

## Verification

Deterministic regression fixtures exercise source-selection changes,
changed coverage inputs, stale published profiles, interrupted leftovers,
failed Hub/rest race/rest coverage groups, missing output and overlap.
Mixed-profile tests verify hit/miss normalization and reject invalid set counts.
The real race fixture still fails when coverage collection is enabled.
Existing subprocess lock tests exercise concurrent waiters, FIFO ordering,
canceling a waiter/owner, queue deadlines and recovery after process death.
New fake-time telemetry tests ensure a queue deadline records wait time and
zero active run time.

Worker heartbeats attribute queueing to `local_validation_queue` and clear
that reason after admission. The board shows [Validation queued](queued.png)
and [Validating](running.png), verified in an isolated seeded browser preview.
Browser verification did not change the live service or port 4000.

PR review exposed a missing producer link: command output originally never
became a progress message. A regression reproduced this through real
`AgentUpdateToolOutput` events and `publishRunUpdate`. The runner now forwards
only recognized validation diagnostic lines to the bounded progress message
and recent events, including split/interleaved output and terminal lines.
Per-tool partial buffers are bounded and cleared on completion/turn changes.
Ordinary logs, quoted source, command payloads and final assistant prose keep
their existing handling. The runner-to-usage regression complements the
heartbeat/board consumer tests and seeded browser screenshots.

An operator restart interrupted the first combined measurement. The
[failed output](combined.log) records missing scratch executable and coverage
directories; [gate events](measurement-events.jsonl) record failure after
21m25.967s of queueing and 386.600s of measurement work, including warmup and
earlier successful groups. No failed timing is used to claim improvement.
The ordinary and race baselines passed before the restart. The cleanup/reaping
ordering investigation is tracked separately in
[#2354](https://github.com/digitaldrywood/detent/issues/2354). The replacement
comparison and gate start fresh; no interrupted result authorizes a pass.

## Measured decision

The successful shared-cache comparison warmed each build variant first,
then ran the complete package with Go 1.26.6 and `-count=1`. Race and
combined runs retain parallelism 2 and the 15-minute timeout; ordinary
coverage retains its default parallelism. Raw `shared-*.log`/`shared-*.time`
files and `measurements.json` retain the results.

| Complete Hub invocation | Wall seconds | User + system CPU seconds | Maximum RSS MiB | Coverage |
| --- | ---: | ---: | ---: | ---: |
| Race | 165.79 | 371.60 | 618.4 | — |
| Ordinary coverage | 29.33 | 61.19 | 394.9 | 77.7% |
| Separate total | 195.12 | 432.79 | 618.4 peak | 77.7% |
| Combined race/coverage | 196.39 | 376.97 | 631.7 | 77.7% |

Combined execution removes the second fixture invocation and reduces
measured CPU time by 55.82 seconds (12.9%), with about 13.3 MiB higher
maximum RSS than the separate peak. The sample does **not** demonstrate a
wall-time improvement: combined wall time is 1.27 seconds higher. This is
one warm sample per variant on a contended host, not a statistically
significant throughput claim or an end-to-end gate benchmark. The earlier
isolated-cache baselines were 163.62s race and 32.86s coverage; their combined
run was damaged by restart and is excluded.

Enable `test-race-cover` in the local full gate to remove the demonstrated
duplicate work and lower CPU demand. Keep one FIFO slot and every required
coverage floor/check. Remote CI continues its existing commands and check
identities. Full local gate validation is recorded separately after this
final source and skill content is in place.
