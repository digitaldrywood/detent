# Isolated runtime readiness investigation

The #2736 report records a 120.26-second wait for `/health` during an
all-package gate, followed by a passing focused rerun. It contains neither an
HTTP status nor a request error. The historical root cause remains unconfirmed.

On the unchanged b41fe7ee1 base, the isolated test passed in 1.35 seconds, the
full CLI package passed in 96.464 seconds, and `go test ./... -count=1` passed
(the CLI package took 165.923 seconds under cross-package load). This does not
prove that the intermittent failure is gone.

Investigation ruled out a partial banner read for this fixture: the banner is
written in one call to a locked buffer. Runtime HTTP helpers already own their
transports, package TestMain clears ambient service credentials, and the
fixture uses an ephemeral port, temporary home, memory tracker, and fake runner.
These are code observations, not proof about the historical environment.

`TestStartRunningPublishesStartupSnapshotBeforeProjectStartCompletes` already
holds provisioning and observes HTTP 503 with lifecycle starting, then releases
provisioning and observes HTTP 200 with lifecycle ready. Thus a readiness wait
can legitimately see non-200 responses from a functioning listener.

The new bounded `TestAwaitDashboardTimeoutEvidence` reproduces the diagnostic
ambiguity using the existing HTTP client dependency and virtual time: repeated
503 responses, repeated 401 responses, and request deadlines all exhausted the
outer deadline with identical errors before this change. All three cases were
observed red. The helper now retains the last response status and last request
error independently and preserves `errors.Is(err, context.DeadlineExceeded)`.
Response bodies are not included. No production mechanism or timeout changed.

The isolated runtime test now cancels and joins its goroutine during failure
cleanup, before its temporary home is removed, following the adjacent fixture's
existing pattern. Its successful promotion and cancellation assertions remain.

Ten repetitions of the focused readiness, isolated runtime, HTTP cleanup, and
canceled-dial tests passed; CLI vet passed. Full gate and current-head CI results
are recorded in the issue Workpad. A future recurrence should use the retained
status/error to select the next diagnostic step; do not infer a startup failure
from the outer deadline alone.

The default-concurrency `make check-fast` run failed after 609 seconds solely
in unchanged `internal/project` tests (one-/two-second lifecycle waits), matching
existing issue #2799; that issue received this occurrence. The changed CLI
package passed in 242.967 seconds in that gate. Five affected project tests
passed ten focused repetitions in 0.355 seconds. The full gate is additionally
validated with `GOMAXPROCS=4`, following the bounded-concurrency experiment
already recorded on #2799; this is not a claim that the default-concurrency gate
passed or that the unrelated project-test defect is fixed.
