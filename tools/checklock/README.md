# Validation queue

`make check` passes the repository's Git common-directory lock to checklock,
so all worktrees share one validation gate. Checklock registers a FIFO ticket
before attempting that existing lock. Once registered, a live waiter cannot be
overtaken by later registrations, including when the head waiter is descheduled.
Concurrent registrations are ordered by acquisition of a short queue mutex.

The queue uses two kinds of OS locks supplied by `internal/instancelock`:

- `<lock>.queue.lock` serializes ticket registration, inspection, and pruning.
  It is released before waiting or running validation and is never removed.
- `<lock>.queue/<ticket>.lock` stays locked while its waiter is queued. Only the
  lowest live ticket may attempt the original validation lock. After acquisition
  or cancellation, its waiter lock closes; the next queue operation prunes the
  released entry. The original validation lock stays held until the command exits.

The queue mutex prevents a ticket from being opened or reused between its
liveness inspection and removal. A crashed process releases its OS locks, making
its tickets recoverable without PID checks, age thresholds, or deleting a live
lock. Pruning retries transient deletion failures up to five times with
cancellation-aware polling, allowing an unlocked Windows file handle to finish
closing. Partial receipts left by a crash are also recoverable. Numeric tickets
avoid wall-clock ordering assumptions. The queue admits up to 1024 live waiters;
excess invocations fail explicitly instead of growing it without a bound.

`-wait-timeout` (`CHECK_LOCK_WAIT` in Make, 15 minutes by default) bounds
registration and waiting without progress. An observed change in valid owner
identity (PID, hostname, acquisition time), or acquisition after an observed
clear lock, renews that budget. Forward queue movement also renews it, including
cancellations ahead while an owner holds the gate. Fresh output activity from
the same identified owner renews the budget without requiring a handoff. An
unchanged holder, phase hints alone, unreadable metadata, periodic reports, and
new arrivals do not renew it. Progress must be observed before the current
budget expires; neither liveness nor output alone proves validation is healthy.

`-max-wait-timeout` (`CHECK_LOCK_MAX_WAIT` in Make, 4 hours by default) bounds
total registration and queue waiting even with repeated handoffs, output, or
queue advancement. This also
bounds contention with older clients that do not honor FIFO. Both durations
must be positive. The waiter retains its original ticket across renewals.
Interrupt/termination signals and parent deadlines cancel waiting. Once
validation starts, neither wait budget limits execution. Parent cancellation and
termination signals stop the active command through the shared process-group
helper. The wrapper retains the lock until command exit, output draining, and
available process-group cleanup finish. Unix output draining has a five-second
post-exit bound followed by descendant cleanup. Windows has no descendant
cleanup, so output draining is not timed out: inherited output handles retain
the gate until they close, including after leader exit or parent cancellation.
The validation command controls its own active timeouts.

Waiting diagnostics report queue position, queue size, elapsed wait, and the
active owner's PID/acquisition time when available. Owner-bound `<lock>.activity`
metadata adds a phase hint inferred from known command output, the latest
activity timestamp, and output byte count. It stores no output body and is
published atomically, at most once per second unless the phase changes. Metadata
from another owner, malformed metadata, and future activity are ignored. Missing
or unreadable activity is explicitly reported as unknown. Reports refresh when
the queue, owner, or phase changes and at least every 30 seconds while polling. Renewal messages identify the observed progress and both budgets. Expiration
messages distinguish no progress, the total cap, and parent cancellation or
deadline expiry, identify waiting before validation has started, and explain
that a retry joins the queue tail. Normal
diagnostics omit repository paths and owner hostnames.

Older checklock binaries still contend on the original exclusive validation
lock, so exclusivity is preserved during rollout. FIFO ordering applies to
invocations using this queue protocol; older polling binaries cannot honor it.
Owners without activity metadata and silent owners retain the idle timeout;
`CHECK_LOCK_WAIT=1h` remains available for a longer bounded wait during rollout.

Validation:

```sh
go test -race ./tools/checklock ./internal/instancelock -count=10
GOOS=windows GOARCH=amd64 go test -c -o tmp/checklock-windows.test.exe ./tools/checklock
make check
```
