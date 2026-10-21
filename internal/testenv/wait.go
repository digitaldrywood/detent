package testenv

import (
	"context"
	"time"
)

const waitPollInterval = 5 * time.Millisecond

// TB is the part of testing.TB the wait helpers use; *testing.T and
// *testing.B satisfy it.
type TB interface {
	Helper()
	Context() context.Context
	Fatalf(format string, args ...any)
}

type deadliner interface {
	Deadline() (time.Time, bool)
}

// WaitContext returns ctx bounded by the go test -timeout deadline, ending
// shortly before the binary's timeout panic so a waiting test can still report
// its own failure message. It never adds a fixed wall-clock budget.
func WaitContext(t TB, ctx context.Context) context.Context {
	t.Helper()
	deadline, ok := testDeadline(t)
	if !ok {
		return ctx
	}
	deadline = deadline.Add(-time.Until(deadline) / 20)
	bounded, cancel := context.WithDeadline(ctx, deadline)
	context.AfterFunc(t.Context(), cancel)
	return bounded
}

// testDeadline reports the go test -timeout deadline. Inside a
// testing/synctest bubble, where Deadline panics and time is fake, it reports
// none: the bubble's own deadlock detection ends a stuck wait instead.
func testDeadline(t TB) (deadline time.Time, ok bool) {
	d, isDeadliner := t.(deadliner)
	if !isDeadliner {
		return time.Time{}, false
	}
	defer func() {
		if recover() != nil {
			deadline, ok = time.Time{}, false
		}
	}()
	return d.Deadline()
}

// WaitUntil returns as soon as cond reports nil. It fails the test with cond's
// last error once ctx ends; ctx is normally t.Context(), which WaitUntil bounds
// by the go test -timeout deadline. Call it only from the test goroutine.
func WaitUntil(t TB, ctx context.Context, cond func() error) {
	t.Helper()
	ctx = WaitContext(t, ctx)
	err := cond()
	if err == nil {
		return
	}
	ticker := time.NewTicker(waitPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("%v (wait ended: %v)", err, context.Cause(ctx))
			return
		case <-ticker.C:
		}
		if err = cond(); err == nil {
			return
		}
	}
}
