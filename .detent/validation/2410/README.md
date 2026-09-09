# Workspace reap verification diagnostics

## Baseline

On macOS, unchanged head `525d2915` reproduced three failures in twelve cases:

```sh
env -u DETENT_API_TOKEN go test -race ./internal/runner -run '^TestRunnerTerminalSessionReapsEscapedWorkspaceProcess$' -count=3 -v
```

Two session-cancellation cases reported `workspace reap = (1, verify workspace processes: signal: killed)`; one completed nested-directory case returned the same verification failure from Run. Total runner package duration: 148.456s. This reproduces the issue symptom, but the original error does not distinguish context cancellation from an independent signal.

## Instrumented results

The first three focused race repetitions passed all twelve cases. Successful reaping took 7.2–8.0s against the unchanged ten-second cleanup budget. Every post-reap snapshot recorded `holder_exited=true observer_exited=false`.

A full workspace/runner race run passed workspace (179.982s), while runner failed in other fixtures. Two artifact-cleanup failures already tracked by #2398 now reported:

```text
scan workspace processes: workspace scan command stage=wait pid=23264 start_elapsed=8.385083ms wait_elapsed=4.553818917s deadline_set=true deadline_remaining=4.559731917s context_at_start=<nil> context_error=context deadline exceeded output_bytes=0: signal: killed
scan workspace processes: workspace scan command stage=wait pid=23301 start_elapsed=5.7585ms wait_elapsed=4.4944855s deadline_set=true deadline_remaining=4.498127334s context_at_start=<nil> context_error=context deadline exceeded output_bytes=0: signal: killed
```

These establish deadline cancellation while waiting for lsof in those artifact fixtures. A separate cancelled scratch-cleanup failure was filed as Backlog #2411.

## Target failure under concurrent race load

A focused target run alongside the repository gate reproduced two verification failures (55.088s package duration):

```text
workspace reap: elapsed=10.000640875s count=1 error=verify workspace processes: workspace scan command stage=wait pid=85749 start_elapsed=2.649375ms wait_elapsed=1.940652291s deadline_set=true deadline_remaining=1.942808584s context_at_start=<nil> context_error=context deadline exceeded output_bytes=0: signal: killed holder_exited=true observer_exited=false
workspace reap: elapsed=10.002503083s count=1 error=verify workspace processes: workspace scan command stage=wait pid=87426 start_elapsed=3.199459ms wait_elapsed=4.127730292s deadline_set=true deadline_remaining=4.128606416s context_at_start=<nil> context_error=context deadline exceeded output_bytes=0: signal: killed holder_exited=true observer_exited=false
```

The failures occurred in session cancellation and completed nested-directory cases. They demonstrate expiry of the shared cleanup budget during verification, after the known workspace holder had exited and while the outside observer remained alive. The OS launch call was short, and no PID output was available from the canceled verification command. This does not prove the workspace had no other descendants, nor attribute the historical incident or the underlying scan slowness to a specific host mechanism. The scope remains diagnostic; cleanup policy and deadlines are unchanged.

## Full-gate and scan-cost observations

The unconstrained `make check` passed lint and reproduced the target timeout during the race gate: Start 2.67ms, Wait 3.36s until deadline cancellation, total reap 10.002s, holder exited and observer alive. It also failed in #2398 artifact fixtures and scratch-cleanup fixtures tracked in #2411. No test assertions were relaxed.

Four sequential read-only lsof probes against an isolated empty temporary directory took 3.682s and 3.674s with existing flags, and 3.666s and 3.656s with additional `-nP`. Every probe exited 1 with no matches. Disabling name resolution did not explain or improve the scan cost, so those options were not added.

The final local gate is run as `GOMAXPROCS=2 make check` to reduce host concurrency while retaining every build, lint, vet, NilAway, race, and coverage step with unchanged test deadlines. The unconstrained failures remain evidence; a constrained passing gate does not establish that the underlying flake is fixed.

## Diagnostic contract

- Errors retain the original command error for `errors.Is`/`errors.As`, including lsof exit-status handling and partial output behavior.
- Start duration measures `exec.Cmd.Start`; wait duration includes executable initialization and scanning. Neither proves loader pressure or the time at which lsof begins scanning.
- Deadline remaining is sampled before Start; context state is sampled before Start and after failure. No raw stdout, environment, or process arguments are added to production errors.
- The runner fixture records reap elapsed time and both known helpers' exit-channel states before fatal assertions. These are instantaneous observations; a false exit snapshot alone does not prove a surviving descendant. Existing completion and lock assertions remain authoritative.
- The existing pre-reap lsof diagnostic probe now has its own five-second bound; production cleanup selection, signals, grace periods, and total budget are unchanged.
- Stdlib table-driven command tests use pipe readiness before cancellation, with no timing sleeps. They distinguish pre-start cancellation/deadline, missing executable, exit status, partial output, an uncanceled signal, and cancellation after readiness.

`GOMAXPROCS=2 make check` passed all steps and configured coverage floors in 14m20.9s. Runner passed race (22.596s) and coverage (86.4%); workspace passed race (112.342s) and coverage (73.9%). Current-head CI results are recorded in the issue Workpad and `.detent/notes.md`.
