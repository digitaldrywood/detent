# Local-progress WaitDelay investigation (#2702)

## Recorded evidence and limits

The incident reported during #2680 was one failure of
`TestSessionLocalCommitProgress/tracked_implementation_edit` after 6.19s,
with the runner package failing after 35.343s:

```text
git diff stat: git -C <fixture>/workspaces/local-progress diff --stat HEAD failed: exec: WaitDelay expired before I/O complete
```

The isolated rerun passed in 2.149s. The #2680 Workpad also records a later
passing full gate. Neither report provides the child's PID, exit timestamp,
pipe holders, or goroutine trace. Host contention remains a hypothesis.

## Completion path

At baseline `aba17cd3`, the failure site is the initial `probe(t.Context())`
in `internal/runner/local_progress_test.go`, before `tt.change` edits the file.
The snapshot calls `LocalGit.RecoveryState`, then `GitDiffStat`, and ultimately
`runGitAtWithEnv` for `diff --stat HEAD`.

`runGitAtWithEnv` uses `exec.CommandContext`, `CombinedOutput`, and the existing
one-second `workspaceCommandWaitDelay`. It uses the standard command cancel
function; this path does not invoke the workspace process-group reaper.

In the inspected Go 1.27.1 `os/exec` implementation, `Wait` first waits for the
process, then joins the output-copying goroutine. With an uncanceled context,
`ErrWaitDelay` means the child exited successfully but the output join selected
the expired timer. The timer closes the parent pipe and joins the copier before
returning. A nonzero child exit takes precedence over that error. The workspace
wrapper substitutes the context error if cancellation is present.

Thus the error is evidence of a post-exit output-drain timeout, not proof that
Git itself ran beyond a deadline. The 6.19s subtest duration includes fixture
setup and other Git commands; it is not the duration of this single command.
An inherited pipe can produce the error, but the original report does not
identify a descendant. Delayed goroutine scheduling also remains unmeasured.

## Reproduction attempts

Environment: macOS arm64, Go 1.27.1, Git 2.54.0. No cache, Git configuration,
test parallelism, or command-timeout overrides were introduced.

```sh
go test ./internal/runner -run '^TestSessionLocalCommitProgress$' -count=20
go test ./internal/workspace -run '^TestRunGitAtBoundsInheritedOutput$' -count=1 -v
go test ./internal/runner -count=3
make check-fast
```

- All 20 local-progress repetitions passed in 26.846s, including all ten
  scenarios and the tracked-edit assertion.
- The existing retained-output regression passed in 3.855s. Its FIFO-controlled
  descendant retains stdout or stderr after the fake Git parent exits zero;
  both cases return `ErrWaitDelay`. A nonzero exit remains an exit error, and
  redirecting both output streams permits successful completion. The fixture
  releases its descendant, waits for its acknowledgment, and joins the command
  goroutine.
  This reproduces the error class, **not the original real-Git failure**.
- Three complete runner-package repetitions passed in 78.855s while the full
  gate was also running. This exercises sibling tests as well as local progress;
  it does not recreate or quantify the historical host load.
- First full gate: workspace passed in 544.257s and runner in 36.744s, with no
  recurrence of the reported failure. The gate exited 2 because
  `TestRunDoctorSuppressesConnectorLogsFromProgress` failed in CLI: the expected
  readiness check was missing after `Project alpha checks` timed out after 1s
  while running `Project alpha invariant evidence`. That unchanged test passed
  in isolation. The separate failure is tracked in #2711.
- The unchanged full-gate rerun passed (exit 0), including invariants,
  migration/generated checks, build, lint, vet, workspace, and all remaining
  packages. Workspace's test binary reported 370.423s. Neither full run
  reproduced the original local-progress failure. Race, coverage, and nilaway
  remain merge-queue checks under the dispatch contract.

## Disposition

No reproducible process-cleanup defect has been established. Preserve the
tracked-edit regression and existing process handling unchanged; adding a
timeout, retry, or cleanup path is not supported by this evidence.

If the real-Git failure recurs, retain the raw test output and instrument an
isolated reproduction for command start, child exit, context state, and output
drain completion. Correlate pipe ownership and goroutine stacks with those
events before choosing a fix. A retained-output fixture alone cannot establish
the cause of a failure under host load.
