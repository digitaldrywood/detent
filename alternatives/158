# Worktree prune I/O wait investigation

Baseline: `15993b0d` (includes reported baseline `3b1d2dae`). Host: macOS,
Go 1.26.6, `/opt/homebrew/bin/git`. Before the full gate, the branch was
fast-forwarded to `3962d001`; its merge-reservation change does not overlap
the regression.

## Evidence and limits

`initRunnerSourceRepo` executes init, local config, add, and commit serially
through `exec.CommandContext(...).CombinedOutput()`. The workspace then calls
Git directly with `-C <source> worktree prune`; there is no fixture shell or
background helper on this path. CLI TestMain clears inherited `GIT_*` variables.
The inspected user Git configuration had no fsmonitor, maintenance, gc.auto,
or hooksPath override. These are current observations, not historical proof.

Go's `os/exec.Cmd.awaitGoroutines` starts its WaitDelay timer after process wait
when output copying has not completed. Expiration closes the read pipes and
returns `exec.ErrWaitDelay`. With a live context and this error, the direct
process exited successfully, but the copy goroutine did not finish within the
one-second bound. This is not evidence that `git worktree prune` itself timed
out. A descendant retaining stdout/stderr can cause it; delayed scheduling of
the I/O completion is another possibility. The original report contains no
process/descriptor trace to distinguish them.

## Reproduction results

- Original focused CLI test under coverage, 30 consecutive runs: PASS (7.566s).
- Full CLI coverage with a PATH wrapper that logs PID and arguments then execs
  the absolute Git executable: PASS (55.371s, 77.5% coverage).
- Trace: 726 Git launches, including four worktree prune calls. No wait failure.
  The wrapper records invocations, not every descendant or descriptor owner.
  Disposable raw logs remain in the Detent per-turn temp directory.

The historical failure did not reproduce. No timeout increase, retry, ignored
error, or production behavior change is justified by this evidence.

## Deterministic regression

`TestRunGitAtBoundsInheritedOutput` installs a temporary shell Git fixture.
A descendant retains selected output descriptors and waits on a FIFO that the
test owns. The command exits before the test releases the descendant. Cases
cover retained stdout, retained stderr, a nonzero exit, and redirected child
output. Assertions verify ErrWaitDelay, CommandError output preservation,
nonzero exit preservation, and success when the descendant closes output.
Cleanup releases the FIFO and joins the command even when assertions fail.
The test skips Windows because its fixture uses POSIX shell and mkfifo.

Three focused coverage repetitions passed. Temporarily setting only
runGitAtWithEnv's WaitDelay to zero made the retained-stdout case fail at its
ten-second guard (10.295s total), then cleanup completed. Production code was
restored immediately. This is a regression against removing bounded cleanup,
not a claim to reproduce the unknown historical host cause.

## Validation

Focused race regression, three repetitions: PASS (12.315s).
`make check CHECK_LOCK_WAIT=2h`: PASS, overall coverage 80.3%, CLI 77.5%,
workspace 73.0%; package and exact-file floors passed. Lint, vet, NilAway,
and the full race suite passed. The first plain `make check` exited before
validation started because its 15-minute shared-lock wait expired. The retry
waited about 50 minutes for the same lock; test timeouts and gate steps were
unchanged. Current-head PR CI is tracked in the issue Workpad.

Skill draft: no — existing subprocess-pressure-tracing and inherited-pipe
regressions already describe this method; no new reusable method was found.
