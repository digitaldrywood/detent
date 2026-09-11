# Installer coverage timeout investigation

## Restoration on September 11, 2026

Operator authorization is recorded in #2408. The installer helper on current
main (`9fa6fe3a`) is identical to the pre-patch helper. Restored only the installer
test diagnostics from `f03d5962`; no production code or installer fixtures changed.
The historical measurements below are preserved from that commit, not rerun
or represented as validation of the restored head.

Current validation:

- Controlled cancellation/deadline assertions failed on current main (exit 1):
  both errors lacked the expected process IDs and executable evidence.
- `env -u DETENT_API_TOKEN go test . -run
  '^(TestRunInstallerCommand|TestInstallerProcessGroup|TestInstallScript)' -count=1`
  passed in 1.387s after restoration.
- `env -u DETENT_API_TOKEN make check-fast` passed, including repository-wide
  tests and the invariant gate. Independent validator: approve, score 9/10,
  no P1/P2 findings. Race, coverage, and nilaway
  are reserved for the merge queue by the worker protocol.

## Finding

The historical ten-second installer stalls did not recur in this investigation.
Their nested command and host-level cause remain unestablished. Successful
installation output alone does not establish that the shell has exited: service
probes, PATH reporting, and EXIT cleanup follow installation. No deadline,
fixture, installer behavior, or test scheduling changes are justified by the
available evidence. For an invocation that already printed PATH guidance,
service probing had returned and EXIT cleanup was the remaining script stage.
That narrows where to sample, but does not establish that `rm` was blocked or
identify a host-level cause.

The existing failure report identifies shell startup, waiting, and output-pipe
draining, but discards the live process group when cancellation kills it. A
readiness-triggered cancellation regression reproduced this diagnostic gap for
both cancellation and deadline causes. The old helper reported, for example:

```text
command "/bin/sh" args=["-c" "printf ready; read line <&3"] pid=90155 stage=wait start=1ms wait=2ms: signal: killed
context deadline exceeded
```

It could not identify any live child at the time of cancellation.

## Historical measurements (prior removed patch)

Native macOS arm64, starting revision `3962d001`:

- Unmodified `env -u DETENT_API_TOKEN go test -coverprofile=tmp/install-baseline.out ./...`
  passed. Root package: 24.993 seconds, 75.0% coverage.
- A second whole-repository coverage run traced installer external commands via
  temporary PATH wrappers, preserving package and test parallelism and existing
  subprocess deadlines. Root package: 19.937 seconds, 75.0% coverage; all traced
  commands completed. Command arguments and environment values were not logged.
- Traced commands included ten `launchctl`, thirteen `rm`, fifteen `curl`, twelve
  `mktemp`, eleven `install`, and eighteen `mkdir` invocations. No unfinished
  command established a culprit. Wrappers add process overhead, so this is not
  a timing comparison against baseline.
- That traced run failed three unrelated one-second PID-file readiness checks:
  `TestRunTurnCancellationKillsChildProcessGroup`,
  `TestLocalTransportCloseKillsChildProcessGroup`, and
  `TestLocalTransportReapsChildAfterParentExits`. All passed 25 subsequent
  unwrapped focused repetitions and five focused repetitions with the same
  tracing environment. This does not establish whether the wrapper or concurrent
  host pressure caused those failures. Follow-up: Backlog issue #2380.
- With the final diagnostic code, an uncached whole-repository run
  (`env -u DETENT_API_TOKEN go test -count=1 -coverprofile=tmp/install-instrumented.out ./...`)
  passed without wrappers. Root package: 13.799 seconds, 75.0% coverage. This
  reran every package rather than relying on results cached by earlier runs.
- New cancellation/deadline evidence assertions failed against the old helper.
  Five focused race repetitions passed after adding process evidence, including
  installer success, target selection, authentication, redirect, fallback,
  startup failure, exit failure, cancellation, and output-drain coverage.
- An additional five race repetitions passed with a readiness-triggered nested
  shell regression that checks the child's parent and process-group IDs before
  cleanup. Existing inherited-descriptor checks still verify descendant exit.

## Diagnostic change

On cancellation, the test helper runs `ps -axo pid=,ppid=,pgid=,stat=,comm=`
before killing its isolated process group. It retains only that group's process
IDs, parent IDs, group IDs, states, and executable names. It does not collect
command arguments or environment values from the process table. Failed waits
also sample before remaining descendants are cleaned up.

The diagnostic command has a separate one-second context and one-second output
wait bound. Diagnostic failure is recorded as unavailable; it does not replace
the original command error or cancellation cause. Sampling can add cleanup
latency after failure. Successful commands do not launch a diagnostic process.
The original installer deadline and process-group cleanup remain in place.
If the snapshot observed a live, non-zombie group member and the shell exits
successfully during sampling, preserve the original cancellation cause. If the
group had already exited, consisted only of zombies, or could not be sampled,
retain the original wait result: cancellation can legitimately lose the race
with successful completion. Controlled regressions release the blocked shell
either before or after taking a snapshot and wait for it to be reaped before
returning the snapshot. The first regression catches false success after a live
snapshot; the second catches false cancellation after an empty snapshot.
Automated review of the initial PR identified the latter edge case, which the
new regression reproduced before the correction.

Table-driven tests cover group filtering, malformed/empty rows, spaced executable
names, orphaned children, zombie-only groups, and live nested-command capture. This improves the next
failure's evidence; it is not a demonstrated fix for the historical timeout.

## Next failure

Retain the complete `processes` field alongside stage, start/wait durations,
stdout, and stderr. A live nested executable identifies where to investigate;
its process state is a snapshot, not proof of a persistent host bottleneck.
If sampling is unavailable or the group has already exited, do not infer a
culprit. Preserve whole-repository scheduling while collecting further evidence.
