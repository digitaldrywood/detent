# Serial workspace package budget (#2962)

## Recorded failure

The #2948 local gate on base `b3fc9cc11` exhausted the package timeout at
1200.268 seconds. Its saved `workspace-test-evidence/combined.jsonl` contains
529 passing tests/subtests, no failing individual test, and only the
`TestCheckpointRefusesUnsafePublication` parent and five children unfinished.
`unapproved_commit` had just continued; `stale_head`, `different_branch`,
`secret_path`, and `secret_content` were waiting for the serial slot.
This is cumulative workload evidence, not evidence of a hung last test.
The shared gate lock wait is outside the package timer.

The largest completed group was `TestFilesystemCleanupBatch` (107.72s).
Its 0/1/10/11/50-candidate cases check the ten-removal boundary using real
filesystem cleanup and process inventory. The 10/11/50 cases took
33.58/36.01/33.95s. Other completed groups included
`TestLocalGitReconcileResiduals` (31.49s),
`TestReapScratchProcessesOutsideWorkspace` (19.61s), and
`TestGitFixtureEnvironmentIsolation` (15.32s). Parent and child elapsed
values overlap and must not be added to estimate package duration.

## Fixture review and budget

The common source fixture already constructs its identical Git history once
per process and copies independent files for each caller. Keeping those copies
avoids sharing mutable refs, configuration, indexes, or Git objects between
cases. Cleanup/preservation/publication fixtures exercise real Git commands and
process scans; this change does not replace those integration checks with mocks
or reduce the batch-boundary workloads.

The shared runner now allows 30 minutes, 50% above the observed 20-minute
exhaustion near the end of the suite. This replaces the previous budget based
on a 992-second sample (#2646). A historical 460.645-second run (#2686) and the
current measurements show why a single fast sample is insufficient headroom.
This is a bounded package allowance, not a claim that every host load fits it.

Serial parallelism, test selection, assertions, and operation/hook/process
scan deadlines are unchanged. Ordinary, race, and coverage invocations already
share this script; CI job-level deadlines remain independent. No production
mechanism or repository invariant changes.

## Validation

Full baseline measurement and configured gate results are recorded in the
Workpad and PR. The baseline uses an uncached serial JSON run with a diagnostic
40-minute ceiling so a complete sample can be collected without altering any
individual operation deadline. Race and coverage run in the merge queue.

The unchanged full serial baseline passed in **959.155 seconds**
with 535 passing test/subtest events on go version go1.27.1 darwin/arm64.
The five children unfinished in the original timeout took 13.05 seconds
in this sample. These timings cannot reconstruct the original host load, but
confirm the remaining workload was bounded and all tests complete. See
[timing-summary.json](timing-summary.json) for compact measured evidence.

`bash -n scripts/test-workspace.sh` and `go vet ./internal/workspace/...` passed.

The first configured gate's workspace run also passed in **208.223 seconds**,
recording `package_timeout: 30m0s` and `test_parallelism: 1`. The gate then
failed two root-package runner tests that still expected `-timeout 20m`:
`TestCIRaceShardFailures/workspace_shard` and
`TestCombinedCoveragePublishesOnlyCurrentSuccessfulRun/pass`. Their fake-Go
argument checks now require 30m; all root-package tests (`go test . -count=1`)
and `go vet .` pass. The final gate is rerun after fixing that specific cause;
its outcome is recorded in the Workpad/PR. The first gate waited 46m31.845s
for the shared lock and ran 11m14.254s; lock time is not package runtime.
